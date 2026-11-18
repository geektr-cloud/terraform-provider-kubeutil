package tunnel

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTelnetProbeWindow(t *testing.T) {
	t.Parallel()

	window, err := telnetProbeWindow(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if window != time.Second {
		t.Fatalf("window = %s, want 1s", window)
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	window, err = telnetProbeWindow(shortCtx)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if window > 50*time.Millisecond || window <= 0 {
		t.Fatalf("window = %s, want it bounded by the context deadline", window)
	}

	expiredCtx, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expiredCancel()
	if _, err := telnetProbeWindow(expiredCtx); err == nil {
		t.Fatal("expected an error when no time remains")
	}
}

func TestCheckTelnetProbeTiesProbeToAttempt(t *testing.T) {
	t.Parallel()

	first := make(chan struct{})
	second := make(chan struct{})
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()

	var snapshots int
	snapshot := func() chan struct{} {
		snapshots++
		if snapshots == 1 {
			return first
		}
		return second
	}

	err := checkTelnetProbe(context.Background(), "127.0.0.1:1", func(context.Context) (net.Conn, error) {
		return client, nil
	}, snapshot, time.Second)
	if err == nil || !strings.Contains(err.Error(), "forwarding attempt changed") {
		t.Fatalf("expected an attempt-change error, got %v", err)
	}
}

func TestCheckTelnetProbeNotEstablished(t *testing.T) {
	t.Parallel()

	err := checkTelnetProbe(context.Background(), "127.0.0.1:1", func(context.Context) (net.Conn, error) {
		t.Fatal("dial must not be attempted while the tunnel is not established")
		return nil, nil
	}, func() chan struct{} { return nil }, time.Second)
	if err == nil || !strings.Contains(err.Error(), "not currently established") {
		t.Fatalf("expected a not-established error, got %v", err)
	}
}

func TestCheckTelnetProbeDialFailure(t *testing.T) {
	t.Parallel()

	lost := make(chan struct{})
	err := checkTelnetProbe(context.Background(), "127.0.0.1:1", func(context.Context) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}, func() chan struct{} { return lost }, time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not accept a connection") {
		t.Fatalf("expected a dial error, got %v", err)
	}
}

func TestCheckTelnetProbeDetectsLossDuringWindow(t *testing.T) {
	t.Parallel()

	lost := make(chan struct{})
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(lost)
	}()

	err := checkTelnetProbe(context.Background(), "127.0.0.1:1", func(context.Context) (net.Conn, error) {
		return client, nil
	}, func() chan struct{} { return lost }, time.Second)
	if err == nil || !strings.Contains(err.Error(), "closed during the observation window") {
		t.Fatalf("expected a loss error, got %v", err)
	}
}

func TestCheckTelnetProbeSurvivesWindow(t *testing.T) {
	t.Parallel()

	lost := make(chan struct{})
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()

	err := checkTelnetProbe(context.Background(), "127.0.0.1:1", func(context.Context) (net.Conn, error) {
		return client, nil
	}, func() chan struct{} { return lost }, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("expected the probe to pass, got %v", err)
	}
}

func TestCheckTelnetProbeHonorsCancellation(t *testing.T) {
	t.Parallel()

	lost := make(chan struct{})
	client, server := net.Pipe()
	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := checkTelnetProbe(ctx, "127.0.0.1:1", func(context.Context) (net.Conn, error) {
		return client, nil
	}, func() chan struct{} { return lost }, time.Second)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a cancellation error, got %v", err)
	}
}
