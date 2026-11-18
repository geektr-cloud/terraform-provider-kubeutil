package kube

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/tunnel"
)

// LeaseTracker keeps process-scoped references for ephemeral resource
// instances. The framework creates ephemeral resource implementations per RPC,
// so the Open/Close bookkeeping must live in provider data instead of the
// resource struct.
type LeaseTracker struct {
	mu     sync.Mutex
	next   atomic.Uint64
	leases map[string]*tunnel.Lease
}

// NewLeaseTracker returns an empty tracker.
func NewLeaseTracker() *LeaseTracker {
	return &LeaseTracker{leases: map[string]*tunnel.Lease{}}
}

// Track stores lease under a fresh token and returns the token plus the tunnel
// key. The token is what the ephemeral resource puts in its private state.
func (t *LeaseTracker) Track(lease *tunnel.Lease) (token string, key string) {
	token = fmt.Sprintf("%d", t.next.Add(1))
	t.mu.Lock()
	t.leases[token] = lease
	t.mu.Unlock()
	return token, lease.Key().String()
}

// Release drops the lease for token. It reports false when the token is
// unknown, which makes Close idempotent and safe across provider restarts.
func (t *LeaseTracker) Release(token string) bool {
	t.mu.Lock()
	lease, ok := t.leases[token]
	if ok {
		delete(t.leases, token)
	}
	t.mu.Unlock()
	if !ok {
		return false
	}
	lease.Release()
	return true
}

// Len reports the number of tracked leases. Intended for tests.
func (t *LeaseTracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.leases)
}

// EphemeralPrivateState is the private state carried from Open to Close.
type EphemeralPrivateState struct {
	Token string `json:"token"`
	Key   string `json:"key"`
}

// Encode serializes the private state as JSON, as required by the framework's
// private state API.
func (s EphemeralPrivateState) Encode() ([]byte, error) {
	return json.Marshal(s)
}

// DecodeEphemeralPrivateState parses private state written by Open.
func DecodeEphemeralPrivateState(raw []byte) (EphemeralPrivateState, error) {
	var state EphemeralPrivateState
	if len(raw) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return EphemeralPrivateState{}, fmt.Errorf("invalid tunnel private state: %w", err)
	}
	return state, nil
}
