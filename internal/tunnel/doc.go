// Package tunnel implements process-scoped Kubernetes port-forward tunnels.
//
// A tunnel is identified by its namespace, canonical target, and normalized
// requested port. It owns one loopback listener, resolves the target to a
// backing pod and a numeric container port, and keeps the forwarding path alive
// across connection loss and pod drift. See the ADRs in docs/adr for the
// rationale behind the public identity and the forwarding strategy.
package tunnel
