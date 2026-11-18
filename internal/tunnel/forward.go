package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	streamhttp "k8s.io/streaming/pkg/httpstream"
)

const (
	// BindAddress is the only address a tunnel ever listens on. Using the
	// literal avoids client-go's "localhost" expansion, which binds both 127.0.0.1
	// and ::1 and tolerates partial bind failure.
	BindAddress = "127.0.0.1"

	// dialGuard bounds connection establishment and response headers for the
	// forwarding upgrade. It is not rest.Config.Timeout: once the connection is
	// upgraded that timeout would kill the long-lived stream.
	dialGuard = 30 * time.Second

	// stopWait bounds how long Stop waits for the keeper after cancellation.
	stopWait = dialGuard + 5*time.Second

	// logTailLimit bounds how much client-go output is retained for classifying
	// permanent listen failures.
	logTailLimit = 4096
)

// Options configures a Tunnel. The provider's rest.Config and clientset are
// process-wide; each tunnel clones the config so it can install its own
// acquisition-only transport guard.
type Options struct {
	Client     kubernetes.Interface
	Config     *rest.Config
	Namespace  string
	Target     Target
	Port       ParsedPort
	Timeout    time.Duration
	LogContext context.Context
}

// Tunnel owns one local listener and its forwarding lifetime, including
// reconnection and target re-resolution.
type Tunnel struct {
	client         kubernetes.Interface
	config         *rest.Config
	namespace      string
	target         Target
	port           ParsedPort
	attemptTimeout time.Duration
	logCtx         context.Context

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	first chan attemptResult

	localPort atomic.Uint32

	mu   sync.Mutex
	lost chan struct{}

	onDead func()

	backoffMu      sync.Mutex
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

type attemptResult struct {
	localPort uint16
	err       error
}

type forwardAttempt struct {
	pf     *portforward.PortForwarder
	ready  <-chan struct{}
	errCh  <-chan error
	errOut *boundedLogWriter
}

// NewTunnel constructs an unstarted tunnel. Registry.Acquire starts it after
// installing process-wide bookkeeping.
func NewTunnel(rpcCtx context.Context, opts Options) *Tunnel {
	if opts.LogContext == nil {
		opts.LogContext = context.Background()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}

	cfg := opts.Config
	if cfg != nil {
		cfg = rest.CopyConfig(cfg)
		inner := cfg.WrapTransport
		cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
			if inner != nil {
				rt = inner(rt)
			}
			return &responseHeaderTimeoutTransport{next: rt, timeout: dialGuard}
		}
		cfg.Dial = (&net.Dialer{Timeout: dialGuard, KeepAlive: 30 * time.Second}).DialContext
	}

	// The keeper must outlive the RPC context: returning from a data source read
	// or ephemeral open must not tear the tunnel down.
	keeperCtx, cancel := context.WithCancel(context.WithoutCancel(rpcCtx))

	return &Tunnel{
		client:         opts.Client,
		config:         cfg,
		namespace:      opts.Namespace,
		target:         opts.Target,
		port:           opts.Port,
		attemptTimeout: opts.Timeout,
		logCtx:         opts.LogContext,
		ctx:            keeperCtx,
		cancel:         cancel,
		done:           make(chan struct{}),
		first:          make(chan attemptResult, 1),
		initialBackoff: time.Second,
		maxBackoff:     30 * time.Second,
	}
}

func (t *Tunnel) setBackoff(initial, maxDelay time.Duration) {
	t.backoffMu.Lock()
	defer t.backoffMu.Unlock()
	if initial > 0 {
		t.initialBackoff = initial
	}
	if maxDelay >= t.initialBackoff {
		t.maxBackoff = maxDelay
	} else {
		t.maxBackoff = t.initialBackoff
	}
}

func (t *Tunnel) backoff() (time.Duration, time.Duration) {
	t.backoffMu.Lock()
	defer t.backoffMu.Unlock()
	return t.initialBackoff, t.maxBackoff
}

func (t *Tunnel) setDeadCallback(fn func()) {
	t.onDead = fn
}

func (t *Tunnel) markDead() {
	t.cancel()
	if t.onDead != nil {
		t.onDead()
	}
}

// Start launches the keeper and waits for the first attempt to become ready.
// The wait is bounded by acqCtx; on failure the tunnel is stopped and never
// cached.
func (t *Tunnel) Start(acqCtx context.Context) error {
	if t.client == nil || t.config == nil {
		t.cancel()
		close(t.done)
		return fmt.Errorf("provider is not configured with a Kubernetes client")
	}

	go t.keep()

	select {
	case result := <-t.first:
		if result.err != nil {
			t.Stop()
			return result.err
		}
		t.localPort.Store(uint32(result.localPort))
		return nil
	case <-acqCtx.Done():
		t.Stop()
		return acqCtx.Err()
	}
}

// Stop cancels the keeper and waits for the listener to be released.
func (t *Tunnel) Stop() {
	t.cancel()
	select {
	case <-t.done:
	case <-time.After(stopWait):
		tflog.Warn(t.logCtx, "kubernetes portforward: timed out waiting for tunnel keeper to exit")
	}
}

// LocalPort returns the OS-assigned local port, or 0 before first readiness.
func (t *Tunnel) LocalPort() uint16 {
	return uint16(t.localPort.Load())
}

// Hostport returns the consumer-facing loopback endpoint.
func (t *Tunnel) Hostport() string {
	return net.JoinHostPort(BindAddress, strconv.Itoa(int(t.LocalPort())))
}

func (t *Tunnel) keep() {
	defer close(t.done)

	initialBackoff, maxBackoff := t.backoff()
	backoff := initialBackoff
	firstPending := true
	reportFirst := func(result attemptResult) {
		if !firstPending {
			return
		}
		firstPending = false
		t.first <- result
	}

	for {
		if err := t.ctx.Err(); err != nil {
			reportFirst(attemptResult{err: err})
			return
		}

		resolved, err := t.resolveOnce()
		if err != nil {
			if t.ctx.Err() != nil {
				reportFirst(attemptResult{err: t.ctx.Err()})
				return
			}
			if firstPending {
				reportFirst(attemptResult{err: err})
				return
			}
			tflog.Warn(t.logCtx, "kubernetes portforward: target resolution failed, retrying", map[string]any{
				"namespace": t.namespace,
				"target":    t.target.Canonical(),
				"error":     err.Error(),
			})
			if !t.waitBackoff(&backoff, maxBackoff) {
				return
			}
			continue
		}

		attempt, err := t.startAttempt(resolved)
		if err != nil {
			// Building the local forwarding transport cannot succeed by retrying.
			reportFirst(attemptResult{err: err})
			t.markDead()
			return
		}

		var (
			bound  uint16
			runErr error
			ready  bool
		)
		select {
		case <-attempt.ready:
			ports, portErr := attempt.pf.GetPorts()
			switch {
			case portErr != nil:
				runErr = portErr
			case len(ports) == 0:
				runErr = fmt.Errorf("portforward reported readiness without a forwarded port")
			default:
				bound = ports[0].Local
				ready = true
			}
		case runErr = <-attempt.errCh:
		case <-t.ctx.Done():
			runErr = t.ctx.Err()
		}

		if !ready || runErr != nil {
			if t.ctx.Err() != nil {
				reportFirst(attemptResult{err: t.ctx.Err()})
				return
			}
			failure := runErr
			if failure == nil {
				failure = fmt.Errorf("portforward failed before becoming ready")
			}
			output := attempt.errOut.Text()
			if output != "" {
				failure = fmt.Errorf("%w: %s", failure, output)
			}
			if firstPending {
				reportFirst(attemptResult{err: failure})
				return
			}
			if isPermanentListenFailure(output) {
				tflog.Error(t.logCtx, "kubernetes portforward: cannot re-bind the tunnel's local port, marking it dead", map[string]any{
					"namespace":  t.namespace,
					"target":     t.target.Canonical(),
					"local_port": t.LocalPort(),
					"error":      failure.Error(),
				})
				t.markDead()
				return
			}
			tflog.Warn(t.logCtx, "kubernetes portforward: forwarding attempt failed, retrying", map[string]any{
				"namespace": t.namespace,
				"target":    t.target.Canonical(),
				"error":     failure.Error(),
			})
			if !t.waitBackoff(&backoff, maxBackoff) {
				return
			}
			continue
		}

		t.localPort.Store(uint32(bound))
		lost := make(chan struct{})
		t.setLost(lost)
		backoff = initialBackoff
		reportFirst(attemptResult{localPort: bound})

		tflog.Info(t.logCtx, "kubernetes portforward: tunnel established", map[string]any{
			"namespace":   t.namespace,
			"target":      t.target.Canonical(),
			"pod":         resolved.PodName,
			"local_port":  bound,
			"target_port": resolved.TargetPort,
		})

		select {
		case runErr = <-attempt.errCh:
		case <-t.ctx.Done():
			runErr = t.ctx.Err()
			// ForwardPorts closes its listeners before returning; wait for that
			// so Stop observers see a released port.
			if drainErr := <-attempt.errCh; drainErr != nil {
				tflog.Debug(t.logCtx, "kubernetes portforward: attempt ended while stopping", map[string]any{
					"error": drainErr.Error(),
				})
			}
		}

		t.clearLost(lost)
		close(lost)

		if t.ctx.Err() != nil {
			return
		}

		output := attempt.errOut.Text()
		tflog.Warn(t.logCtx, "kubernetes portforward: tunnel lost, reconnecting", map[string]any{
			"namespace": t.namespace,
			"target":    t.target.Canonical(),
			"error":     runErr.Error(),
			"backoff":   backoff.String(),
		})
		if isPermanentListenFailure(output) {
			t.markDead()
			return
		}
		if !t.waitBackoff(&backoff, maxBackoff) {
			return
		}
	}
}

func (t *Tunnel) resolveOnce() (Resolved, error) {
	ctx, cancel := context.WithTimeout(t.ctx, t.attemptTimeout)
	defer cancel()
	return Resolve(ctx, t.client, t.namespace, t.target, t.port)
}

func (t *Tunnel) startAttempt(resolved Resolved) (*forwardAttempt, error) {
	restClient := t.client.CoreV1().RESTClient()
	if restClient == nil {
		return nil, fmt.Errorf("kubernetes clientset does not expose a REST client for port forwarding")
	}

	request := restClient.Post().
		Resource("pods").
		Namespace(t.namespace).
		Name(resolved.PodName).
		SubResource("portforward")
	requestURL := request.URL()

	transport, upgrader, err := spdy.RoundTripperFor(t.config)
	if err != nil {
		return nil, fmt.Errorf("failed to build SPDY transport: %w", err)
	}
	spdyDialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, requestURL)

	// WebSocket tunneling is attempted first and falls back to SPDY on upgrade
	// or HTTPS-proxy failures, matching current kubectl behavior.
	websocketDialer, err := portforward.NewSPDYOverWebsocketDialer(requestURL, t.config)
	if err != nil {
		return nil, fmt.Errorf("failed to build WebSocket tunneling dialer: %w", err)
	}
	dialer := portforward.NewFallbackDialer(websocketDialer, spdyDialer, func(err error) bool {
		return streamhttp.IsUpgradeFailure(err) || streamhttp.IsHTTPSProxyError(err)
	})

	out := newBoundedLogWriter(t.logCtx, "stdout")
	errOut := newBoundedLogWriter(t.logCtx, "stderr")
	ready := make(chan struct{})
	ports := []string{fmt.Sprintf("%d:%d", t.LocalPort(), resolved.TargetPort)}

	pf, err := portforward.NewOnAddressesWithContext(t.ctx, dialer, []string{BindAddress}, ports, ready, out, errOut)
	if err != nil {
		return nil, fmt.Errorf("failed to create port forwarder: %w", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- pf.ForwardPorts()
	}()

	return &forwardAttempt{pf: pf, ready: ready, errCh: errCh, errOut: errOut}, nil
}

func (t *Tunnel) waitBackoff(backoff *time.Duration, maxDelay time.Duration) bool {
	timer := time.NewTimer(*backoff)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-t.ctx.Done():
		return false
	}

	*backoff *= 2
	if *backoff > maxDelay {
		*backoff = maxDelay
	}
	return true
}

func (t *Tunnel) setLost(ch chan struct{}) {
	t.mu.Lock()
	t.lost = ch
	t.mu.Unlock()
}

func (t *Tunnel) clearLost(ch chan struct{}) {
	t.mu.Lock()
	if t.lost == ch {
		t.lost = nil
	}
	t.mu.Unlock()
}

func (t *Tunnel) lostSnapshot() chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lost
}

// CheckTelnet performs the optional telnet healthcheck against the tunnel's
// local listener. It sends no application bytes and reports only whether the
// tunnel was rejected during the observation window; it is not proof that the
// remote application is healthy.
func (t *Tunnel) CheckTelnet(ctx context.Context) error {
	address := t.Hostport()
	if t.LocalPort() == 0 {
		return fmt.Errorf("telnet healthcheck: tunnel has no local listener")
	}

	window, err := telnetProbeWindow(ctx)
	if err != nil {
		return err
	}

	dialer := net.Dialer{}
	dial := func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", address)
	}
	return checkTelnetProbe(ctx, address, dial, t.lostSnapshot, window)
}

// telnetProbeWindow returns the observation window, bounded by the remaining
// acquisition timeout.
func telnetProbeWindow(ctx context.Context) (time.Duration, error) {
	window := time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < window {
			window = remaining
		}
	}
	if window <= 0 {
		return 0, fmt.Errorf("telnet healthcheck: no time remains within the acquisition timeout")
	}
	return window, nil
}

// checkTelnetProbe ties the probe to exactly one forwarding attempt. The
// attempt's lost channel is captured before dialing and verified unchanged once
// the connection has been accepted, so a reconnect that happens in between
// cannot be mistaken for a surviving attempt.
func checkTelnetProbe(ctx context.Context, address string, dial func(context.Context) (net.Conn, error), snapshot func() chan struct{}, window time.Duration) error {
	before := snapshot()
	if before == nil {
		return fmt.Errorf("telnet healthcheck: tunnel is not currently established")
	}

	conn, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("telnet healthcheck: local listener %s did not accept a connection: %w", address, err)
	}
	defer func() { _ = conn.Close() }()

	if after := snapshot(); after != before {
		return fmt.Errorf("telnet healthcheck: forwarding attempt changed while the probe connected")
	}

	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case <-before:
		return fmt.Errorf("telnet healthcheck: tunnel closed during the observation window")
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("telnet healthcheck: %w", ctx.Err())
	}
}

// responseHeaderTimeoutTransport bounds the time spent dialing and waiting for
// a response to an upgrade request. It stops the timer on success and does not
// cancel the successful request context, because cancelling it after an upgrade
// would tear down the newly established stream.
type responseHeaderTimeoutTransport struct {
	next    http.RoundTripper
	timeout time.Duration
}

func (t *responseHeaderTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	timer := time.AfterFunc(t.timeout, cancel)
	resp, err := t.next.RoundTrip(req.WithContext(ctx))
	if timer.Stop() {
		if err != nil {
			cancel()
		}
		return resp, err
	}

	cancel()
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return nil, fmt.Errorf("timed out after %s waiting for portforward response headers", t.timeout)
}

type boundedLogWriter struct {
	ctx  context.Context
	name string

	mu   sync.Mutex
	tail []byte
}

func newBoundedLogWriter(ctx context.Context, name string) *boundedLogWriter {
	return &boundedLogWriter{ctx: ctx, name: name}
}

func (w *boundedLogWriter) Write(p []byte) (int, error) {
	if message := strings.TrimRight(string(p), "\r\n"); message != "" {
		tflog.Debug(w.ctx, "kubernetes portforward output", map[string]any{
			"stream":  w.name,
			"message": message,
		})
	}

	w.mu.Lock()
	w.tail = append(w.tail, p...)
	if len(w.tail) > logTailLimit {
		w.tail = w.tail[len(w.tail)-logTailLimit:]
	}
	w.mu.Unlock()

	return len(p), nil
}

func (w *boundedLogWriter) Text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.tail)
}

// isPermanentListenFailure recognizes client-go's listener failure. ForwardPorts
// only returns the generic "unable to listen on any of the requested ports"
// error; the underlying cause is written to errOut. The strings are stable
// parts of client-go's listener error messages.
func isPermanentListenFailure(output string) bool {
	if !strings.Contains(output, "Unable to listen on port") {
		return false
	}
	return strings.Contains(output, "address already in use") ||
		strings.Contains(output, "permission denied") ||
		strings.Contains(output, "cannot assign requested address")
}
