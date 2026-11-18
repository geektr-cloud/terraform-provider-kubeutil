# Changelog

All notable changes to this provider are documented here.

## Unreleased

### Added

- `ephemeral.kubeutil_tunnel`: process-scoped Kubernetes port-forward to an
  abstract target port, with nothing persisted in plan or state.
- `data.kubeutil_tunnel`: same tunnel model for consumers that cannot accept
  ephemeral values. The persisted endpoint is valid only for the provider
  process that read it.
- Target support for built-in kinds (`pod`, `service`, `deployment`,
  `replicaset`, `statefulset`, `daemonset`, `job`, `replicationcontroller`) and
  common kubectl aliases; selector targets resolve to a `Running` pod with
  `targetPort` translation.
- Requested-port support for numbers, port names, and an empty local prefix
  (`:5432`); local ports are always OS-assigned on `127.0.0.1`.
- Provider-instance tunnel registry with reference counting and endpoint sharing
  by namespace, canonical target, and normalized port.
- Reconnection with `1s` to `30s` backoff, target re-resolution, and pod drift on
  the same local port.
- WebSocket-first forwarding with SPDY fallback for proxy compatibility.
- Optional `healthcheck { type = "telnet" }`, a negative rejection check bounded
  by the remaining timeout.
- Provider configuration mirroring the Kubernetes `KUBE_*` environment contract,
  merged in memory through client-go with in-cluster fallback.
