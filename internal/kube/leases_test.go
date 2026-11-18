package kube

import (
	"testing"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/tunnel"
)

func TestLeaseTrackerReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	tracker := NewLeaseTracker()
	lease := &tunnel.Lease{} // zero lease is enough for tracking bookkeeping
	token, key := tracker.Track(lease)
	if token == "" {
		t.Fatal("track returned an empty token")
	}
	if key != "//" {
		t.Fatalf("key = %q, want the zero key", key)
	}
	if tracker.Len() != 1 {
		t.Fatalf("tracker length = %d", tracker.Len())
	}

	if !tracker.Release(token) {
		t.Fatal("first release reported an unknown token")
	}
	if tracker.Release(token) {
		t.Fatal("second release must be a no-op")
	}
	if tracker.Len() != 0 {
		t.Fatalf("tracker length = %d", tracker.Len())
	}
}

func TestEphemeralPrivateStateRoundTrip(t *testing.T) {
	t.Parallel()

	raw, err := EphemeralPrivateState{Token: "7", Key: "default/pod/x/8080"}.Encode()
	if err != nil {
		t.Fatalf("encode failed: %s", err)
	}
	state, err := DecodeEphemeralPrivateState(raw)
	if err != nil {
		t.Fatalf("decode failed: %s", err)
	}
	if state.Token != "7" || state.Key != "default/pod/x/8080" {
		t.Fatalf("unexpected state: %#v", state)
	}

	if _, err := DecodeEphemeralPrivateState([]byte("not-json")); err == nil {
		t.Fatal("expected an error for invalid private state")
	}
	empty, err := DecodeEphemeralPrivateState(nil)
	if err != nil {
		t.Fatalf("nil state must decode: %s", err)
	}
	if empty.Token != "" {
		t.Fatalf("nil state token = %q", empty.Token)
	}
}
