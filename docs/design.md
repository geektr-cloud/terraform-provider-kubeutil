# kubeutil provider design

Design gate for the `geektr-cloud/kubeutil` provider. It records the contract the
implementation must satisfy: what the provider owns, how a tunnel is identified,
when it is opened and released, and which failures are visible to Terraform.

Decisions behind the contract are in [docs/adr](adr/):

- [0001](adr/0001-dual-surfaces-data-source-and-ephemeral.md) — provide both a data source and an ephemeral resource
- [0002](adr/0002-arbitrary-local-port-bound-to-loopback.md) — allocate random local ports on IPv4 loopback only
- [0003](adr/0003-websocket-first-spdy-fallback.md) — prefer WebSocket tunneling with SPDY fallback
- [0004](adr/0004-target-scoped-identity-and-pod-drift.md) — keep tunnel identity target-scoped and allow pod drift
- [0005](adr/0005-telnet-healthcheck-is-negative-detection.md) — make the telnet healthcheck a negative rejection check

## Scope

The provider converts an abstract Kubernetes target port into a loopback TCP
endpoint that Terraform and OpenTofu can consume during the current command.

In scope:

- provider configuration and credential merge
- a process-scoped tunnel registry
- `data.kubeutil_tunnel`
- `ephemeral.kubeutil_tunnel`
- target and requested-port resolution, including port-name to `targetPort` translation
- reconnection and target re-resolution
- an optional negative-rejection healthcheck

Out of scope, deliberately:

- managed resources of any kind
- import, provider functions, and resource state upgrades
- custom resource (CRD) targets
- UDP forwarding
- user-selected local ports
- tunnels that outlive the provider process
- positive confirmation that a remote application is healthy

## Ownership

The provider owns exactly two kinds of state:

1. A local TCP listener bound to `127.0.0.1:<OS-assigned port>`.
2. The in-process forwarding stream and keeper goroutine behind that listener.

It owns no Kubernetes object. It never creates, updates, patches, or deletes
cluster objects, including during `terraform destroy`. A port-forward request to
the `pods/portforward` subresource is transient server-side work; nothing is
left behind when the stream closes or the process exits.

Consequences:

- Destroy is local: release references, close listeners, exit.
- Losing the provider process is a complete cleanup, not a leak.
- No `ImportState` and no drift in the managed-resource sense; the only remote
  interaction is read-only resolution plus the port-forward stream itself.

## Identity

A tunnel is identified by three components:

| Component | Definition |
| --- | --- |
| Effective namespace | Surface `namespace` if set, otherwise the provider default |
| Canonical target | `type/name` after alias normalization; a bare name means `pod/name` |
| Normalized requested port | The user's port spelling, number, or name, with an empty local prefix removed |

The resolved pod and the resolved numeric target port are **not** part of the
identity and are never returned. A tunnel is a stable name for "this target
port", not for the pod currently serving it.

`data.kubeutil_tunnel` exposes that identity as
`id = <namespace>/<canonical-target>/<normalized-port>`.

### Target grammar

Built-in targets only, with common `kubectl` aliases accepted:

- `pod` (`po`)
- `service` (`svc`)
- `deployment` (`deploy`)
- `replicaset` (`rs`)
- `statefulset` (`sts`)
- `daemonset` (`ds`)
- `job`
- `replicationcontroller` (`rc`)

A bare name is `pod/<name>`. CRDs, label selectors, and `all` are rejected.
Selector-based targets (`service`, `deployment`, `replicaset`, `statefulset`,
`daemonset`, `job`, `replicationcontroller`) resolve to a `Running` pod, matching
kubectl's own selection and `targetPort` translation behavior.

### Requested port grammar

- `5432` — numeric
- `postgres` — named container port
- `:5432`, `:postgres` — empty local prefix accepted

A non-empty local component such as `8888:5432` is rejected: the provider always
allocates the local port itself ([ADR 0002](adr/0002-arbitrary-local-port-bound-to-loopback.md)).

## Surfaces and lifecycle operations

| Surface | Framework operation | Tunnel action | Reference lifetime |
| --- | --- | --- | --- |
| `data.kubeutil_tunnel` | Read (invoked on create, refresh, update) | Acquire from registry | Held until process exit |
| `ephemeral.kubeutil_tunnel` | Open | Acquire from registry | Released on Close |

Data sources have no `Delete` operation in the plugin protocol. Their reference
therefore survives until the provider process ends; repeated reads reuse the
same registry entry and do not open a second listener. A changed identity
(namespace, target, or port) acquires a new entry and leaves the old one in
place, since there is no operation that could release it.

Ephemeral resources are paired: `Open` acquires and `Close` releases. The last
release closes the listener and stops the keeper.

Both surfaces report the same outputs: `host` (`127.0.0.1`), `local_port`,
`hostport` (`127.0.0.1:<local_port>`), and `namespace` (the effective namespace).
The data source additionally reports `id`.

### Unknown and null handling

- `target` and `port` are required. If either is unknown at plan time, the
  surface stays unknown and no API call or listener is created.
- `namespace` null means "use the provider default"; it is resolved by the
  provider, not by Terraform.
- `timeout` null means `60s`.
- A missing `healthcheck` block means no healthcheck runs.
- All outputs are unknown until acquisition succeeds.
- An acquisition failure returns a diagnostic and no partial output; it must not
  register a half-built tunnel.

## Defaults and precedence

| Setting | Precedence |
| --- | --- |
| Namespace | surface `namespace` → provider `default_namespace` → credential context namespace (kubeconfig current context, or in-cluster namespace) → `default` |
| Local port | OS-assigned, not configurable |
| Timeout | surface `timeout` → `60s` |
| Transport | WebSocket, then SPDY fallback |
| Healthcheck | none |

`timeout` bounds the whole acquisition: target resolution, pod selection, API
upgrade, listener readiness, and any healthcheck. It does not bound the
lifetime of an established tunnel.

## Credential merge

Configuration is merged in memory through client-go's `clientcmd` machinery.
No temporary kubeconfig file is written, and no credential is copied to disk.

Precedence, highest first:

1. Explicit provider attributes: `host`, `username`, `password`, `token`,
   `client_certificate`, `client_key`, `cluster_ca_certificate`, `tls_server_name`,
   `insecure`, `proxy_url`.
2. `KUBE_*` environment variables: `KUBE_HOST`, `KUBE_USER` (not `KUBE_USERNAME`),
   `KUBE_PASSWORD`, `KUBE_TOKEN`, `KUBE_CLUSTER_CA_CERT_DATA`,
   `KUBE_CLIENT_CERT_DATA`, `KUBE_CLIENT_KEY_DATA`, `KUBE_INSECURE`,
   `KUBE_TLS_SERVER_NAME`, `KUBE_PROXY_URL`, `KUBE_CONFIG_PATH`,
   `KUBE_CONFIG_PATHS`, `KUBE_CTX`, `KUBE_CTX_AUTH_INFO`, `KUBE_CTX_CLUSTER`,
   `KUBE_NAMESPACE`.
3. Kubeconfig files from `config_path` / `config_paths`, with `config_context`,
   `config_context_auth_info`, and `config_context_cluster` selecting context,
   auth info, and cluster.
4. In-cluster service account configuration when nothing else is available.

`default_namespace` additionally reads `KUBE_NAMESPACE`. The `exec` block
(`api_version`, `command`, `args`, `env`) configures an in-memory exec
credential plugin; its `env` values are sensitive for display.

## Tunnel registry

The registry is provider-instance scoped and reference counted, keyed by
effective namespace, canonical target, and normalized requested port.

- Two surfaces with the same key share one listener and one forwarding path.
  They see the same `local_port`.
- Sharing is per provider instance, not global and not per module.
- A shared acquisition still runs its own healthcheck, because the healthcheck
  belongs to the acquiring Terraform instance.
- A data source reference is never released; ephemeral `Close` decrements.
- Process exit releases everything, with no remote cleanup.

## Reconnection and pod drift

The keeper owns forwarding for as long as the registry entry lives:

- On stream failure it reconnects with exponential backoff from `1s` to `30s`,
  retrying indefinitely until the entry is released.
- It re-resolves the target on every reconnect, so a replacement pod or a
  different pod selected by the same service/deployment can take over. This is
  pod drift, and it is expected ([ADR 0004](adr/0004-target-scoped-identity-and-pod-drift.md)).
- The listener stays on the same `127.0.0.1:<local_port>`; the port does not
  change across reconnects.
- Existing forwarded connections are cut and the listener is temporarily down
  while reconnecting. Consumers must tolerate connection resets.
- Even an explicit `pod/name` target tracks the pod name, not a pod UID, so a
  recreated pod with the same name is a legitimate drift target.

## Transport

Forwarding uses client-go's SPDY-over-WebSocket dialer first and falls back to
SPDY when the upgrade or an HTTPS proxy fails
([ADR 0003](adr/0003-websocket-first-spdy-fallback.md)). `proxy_url` (and the
kubeconfig proxy setting) is honored for both paths.

## Healthcheck

`healthcheck { type = "telnet" }` is the only type. It is a negative rejection
check ([ADR 0005](adr/0005-telnet-healthcheck-is-negative-detection.md)):

- It connects to the local listener and sends no application bytes.
- It watches for forwarding failure for up to `1s`, bounded by the remaining
  acquisition timeout.
- A completed local handshake proves only that the listener accepted the
  connection. It does not prove the remote port is open or the application is
  healthy.
- An immediate remote refusal is detected. A delayed refusal can escape the
  observation window and is not reported.
- It runs on every acquisition, including one that reuses a shared tunnel.

With no `healthcheck` block, acquisition succeeds after the target resolves to a
`Running` pod, the API upgrade completes, and the listener is ready.

## Secret exposure

`password`, `token`, `client_key`, and `exec.env` are marked Sensitive.

**Sensitive is display redaction, not encryption.** It affects CLI output only.
Provider configuration is evaluated for each command and is not written to state
by these surfaces, but plan files can contain configuration values; protect plan
artifacts and use encrypted remote state where relevant. The provider never
persists credentials on disk and writes no temporary kubeconfig.

## Data source versus ephemeral resource

| Property | `data.kubeutil_tunnel` | `ephemeral.kubeutil_tunnel` |
| --- | --- | --- |
| Values persisted in plan/state | Yes | No |
| Usable in ordinary resource arguments | Yes | No, ephemeral contexts only |
| Endpoint valid after the command exits | No | No |
| Local port stable across reads | Not guaranteed | Not applicable |
| Opens a real tunnel during plan | Yes | Only in ephemeral evaluation phases |
| Releases its registry reference | Never (process exit) | On `Close` |

Ephemeral values cannot be consumed by a root-module output, even with
`ephemeral = true`. Legal consumers are provisioner and provider configuration
blocks, write-only arguments where the CLI supports them, and other ephemeral
resources.

A persisted data-source result is a record of a port that once existed. On the
next command, or when a saved plan is applied by another process, the old port
is dead. Terraform state is not a tunnel resurrection mechanism: a new read
opens a new tunnel with a new local port, which can appear as a plan diff.

Reading a data source during plan performs real API calls, may list pods, opens a
listener, and can appear in cluster audit logs. That side effect is expected.

## Failure behavior

| Failure | Result |
| --- | --- |
| Unknown `target` or `port` | Surface remains unknown; no side effect |
| Invalid target type, alias, port spelling, or `local:remote` port | Validation error at plan |
| No pod backs a selector target | Wait for a pod to appear, bounded by `timeout` |
| Selected pod is not `Running` | Immediate acquisition error, matching kubectl |
| API authn/authz, upgrade, or proxy failure | Acquisition error with the underlying cause |
| Listener bind failure | Acquisition error; no registry entry retained |
| Healthcheck observes rejection | Acquisition error |
| Stream drops after acquisition | Keeper reconnects with backoff; the Terraform operation is not failed retroactively |
| Reconnect never succeeds | Assigned port remains in the result, but its listener may stay down; only logs report the failure until release or process exit |

RBAC errors are reported as-is; the required verbs are documented in the README.

## Compatibility

| Component | Value |
| --- | --- |
| Module | `github.com/geektr-cloud/terraform-provider-kubeutil` |
| Provider address | `registry.terraform.io/geektr-cloud/kubeutil` |
| Plugin protocol | `6.0` |
| Terraform | `>= 1.10.0` |
| OpenTofu | `>= 1.12.0` |
| Go | `1.26.0` |
| Terraform Plugin Framework | `1.19.0` |
| Terraform Plugin Testing | `1.14.1` |

CLI matrix targeted by CI:

| CLI | Versions |
| --- | --- |
| Terraform | `1.10.0`, `1.17.2` |
| OpenTofu | `1.12.0`, `1.13.9` |

OpenTofu `1.14` is not stable and is intentionally excluded.

## Verification

Verified locally on 2026-11-18:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -race ./...
go mod verify
go mod tidy -diff
golangci-lint run              # v2.15.0
govulncheck ./...
```

The hermetic real-CLI acceptance suite passed on all four matrix versions:
Terraform `1.10.0` and `1.17.2`, OpenTofu `1.12.0` and `1.13.9`. It exercises
actual forwarding, data-source state, ephemeral consumption and Close, and scans
decompressed plan and state artifacts for concrete ephemeral endpoints.

A GoReleaser snapshot built and archived every configured platform with the
standard binary name and valid checksums. Signing was skipped locally because no
release GPG key is available; the release workflow imports it in CI.

CI adds:

- `acceptance`: the same hermetic acceptance tests (`TF_ACC=1`, no cluster)
  against the four CLI versions above, with `TestAccKind` excluded.
- `acceptance-kind`: a real kind cluster with an nginx pod and service, running
  `TestAccKind` on Terraform `1.10.0` and OpenTofu `1.12.0` only, with
  `KUBEUTIL_KIND=1`.

Unit and hermetic tests cover target and port grammar, identity and ID
formatting, credential merge precedence, registry reference counting and
singleflight, reconnect/backoff, healthcheck timing, bidirectional data flow,
WebSocket-to-SPDY fallback, and listener release. The kind tests compile but were
not run locally because the kind node image was unavailable; they remain a CI
acceptance gate rather than a locally verified claim.
