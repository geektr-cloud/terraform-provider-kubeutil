package tunnel

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Key is the target-scoped identity of a tunnel: the effective namespace, the
// canonical target, and the normalized requested port. A resolved pod and a
// translated target port never participate in identity.
type Key struct {
	Namespace string
	Target    string
	Port      string
}

// String returns the stable "<namespace>/<canonical-target>/<port>" spelling
// also used as the data source ID.
func (k Key) String() string {
	return k.Namespace + "/" + k.Target + "/" + k.Port
}

// NewKey builds a key from its components.
func NewKey(namespace string, target Target, port ParsedPort) Key {
	return Key{Namespace: namespace, Target: target.Canonical(), Port: port.String()}
}

// CreateFunc constructs an unstarted tunnel. It is invoked at most once per
// concurrent miss; the registry then starts it and concurrent callers share the
// result.
type CreateFunc func(ctx context.Context) (*Tunnel, error)

type registryEntry struct {
	tunnel *Tunnel
	refs   int
	// call is non-nil while an acquisition is in flight. Waiters block on
	// call.done and reuse call.err on failure.
	call *acquisition
}

type acquisition struct {
	done chan struct{}
	err  error
}

// Registry owns the provider-process-scoped tunnels. It is safe for concurrent
// use, reference counted, and collapses concurrent misses for the same key into
// a single acquisition.
type Registry struct {
	mu      sync.Mutex
	entries map[Key]*registryEntry

	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// NewRegistry returns a registry with the production reconnect backoff.
func NewRegistry() *Registry {
	return NewRegistryWithBackoff(time.Second, 30*time.Second)
}

// NewRegistryWithBackoff allows tests to use shorter reconnect delays.
func NewRegistryWithBackoff(initial, maxDelay time.Duration) *Registry {
	if initial <= 0 {
		initial = time.Second
	}
	if maxDelay < initial {
		maxDelay = initial
	}
	return &Registry{
		entries:        map[Key]*registryEntry{},
		initialBackoff: initial,
		maxBackoff:     maxDelay,
	}
}

// Acquire returns a lease on the tunnel for key, creating it via create on a
// miss. Concurrent misses for the same key share one acquisition. A failed
// acquisition is never cached.
func (r *Registry) Acquire(ctx context.Context, key Key, create CreateFunc) (*Lease, error) {
	for {
		r.mu.Lock()
		entry := r.entries[key]
		if entry == nil {
			entry = &registryEntry{call: &acquisition{done: make(chan struct{})}}
			r.entries[key] = entry
			r.mu.Unlock()

			tunnel, err := r.create(ctx, create, key)

			r.mu.Lock()
			call := entry.call
			if err != nil {
				delete(r.entries, key)
				call.err = err
				close(call.done)
				r.mu.Unlock()
				return nil, err
			}
			entry.tunnel = tunnel
			entry.refs = 1
			entry.call = nil
			close(call.done)
			r.mu.Unlock()

			return &Lease{key: key, tunnel: tunnel, registry: r}, nil
		}

		if entry.call != nil {
			call := entry.call
			r.mu.Unlock()
			select {
			case <-call.done:
				if call.err != nil {
					return nil, call.err
				}
				// The creation succeeded; loop and take a reference. If the
				// entry vanished meanwhile (all references released), the next
				// iteration starts a fresh acquisition.
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		entry.refs++
		tunnel := entry.tunnel
		r.mu.Unlock()

		if tunnel == nil {
			// Defensive: an entry without a tunnel and without an in-flight
			// call cannot serve a lease.
			r.mu.Lock()
			if current := r.entries[key]; current != nil && current.tunnel == nil && current.call == nil {
				delete(r.entries, key)
			}
			r.mu.Unlock()
			return nil, fmt.Errorf("tunnel registry entry for %q is in an invalid state", key.String())
		}

		return &Lease{key: key, tunnel: tunnel, registry: r}, nil
	}
}

func (r *Registry) create(ctx context.Context, create CreateFunc, key Key) (*Tunnel, error) {
	tunnel, err := create(ctx)
	if err != nil {
		return nil, err
	}
	tunnel.setBackoff(r.initialBackoff, r.maxBackoff)
	tunnel.setDeadCallback(func() { r.removeIfCurrent(key, tunnel) })
	if err := tunnel.Start(ctx); err != nil {
		return nil, err
	}
	return tunnel, nil
}

func (r *Registry) release(key Key, tunnel *Tunnel) {
	stop := false

	r.mu.Lock()
	entry := r.entries[key]
	if entry == nil || entry.tunnel != tunnel {
		// The entry was already removed (for example because the tunnel died)
		// or replaced. Never touch the replacement.
		r.mu.Unlock()
		tunnel.Stop()
		return
	}
	entry.refs--
	if entry.refs <= 0 {
		delete(r.entries, key)
		stop = true
	}
	r.mu.Unlock()

	if stop {
		tunnel.Stop()
	}
}

func (r *Registry) removeIfCurrent(key Key, tunnel *Tunnel) {
	r.mu.Lock()
	if entry := r.entries[key]; entry != nil && entry.tunnel == tunnel {
		delete(r.entries, key)
	}
	r.mu.Unlock()
}

// Len reports the number of live tunnel entries. Intended for tests.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Lease is one reference to a shared tunnel. Data sources keep their leases for
// the lifetime of the provider process; ephemeral resources release theirs on
// Close.
type Lease struct {
	key      Key
	tunnel   *Tunnel
	registry *Registry
	once     sync.Once
}

// Tunnel returns the leased tunnel.
func (l *Lease) Tunnel() *Tunnel {
	if l == nil {
		return nil
	}
	return l.tunnel
}

// Key returns the tunnel identity.
func (l *Lease) Key() Key {
	if l == nil {
		return Key{}
	}
	return l.key
}

// Release drops this reference. It is idempotent: repeated calls only release
// once.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.registry == nil {
			// Defensive: a zero lease carries no registry reference.
			return
		}
		l.registry.release(l.key, l.tunnel)
	})
}

// Keys returns the identities currently held by the registry. Intended for
// tests.
func (r *Registry) Keys() []Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]Key, 0, len(r.entries))
	for key := range r.entries {
		keys = append(keys, key)
	}
	return keys
}
