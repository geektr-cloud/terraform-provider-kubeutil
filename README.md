# Terraform/OpenTofu Kubeutil Provider

Open process-scoped Kubernetes port-forward tunnels from Terraform and OpenTofu.

Provider address: `geektr-cloud/kubeutil`

!> **Tunnels live only as long as the provider process.** A tunnel endpoint is
valid for the command and provider process that opened it. It does not survive
that command, and it is never restored from state. Prefer
`ephemeral.kubeutil_tunnel`; use `data.kubeutil_tunnel` only for consumers that
cannot accept ephemeral values, and never treat its persisted endpoint as a
long-lived address.

## Surfaces

| Surface | Purpose |
| --- | --- |
| `ephemeral.kubeutil_tunnel` | Open a tunnel for ephemeral contexts; nothing is persisted |
| `data.kubeutil_tunnel` | Open a tunnel and persist its endpoint; endpoint dies with the process |

There are no managed resources, no provider functions, no CRD targets, and no
import. The provider never creates, changes, or deletes Kubernetes objects: it
only reads target metadata, lists pods for selector targets, and opens a
port-forward to a pod.

## Requirements

- Terraform >= 1.10.0 or OpenTofu >= 1.12.0. The provider defines an ephemeral
  resource, so these are the minimum versions that can load it.
- Network access to a Kubernetes API server, with the RBAC listed below.
- Go 1.26.0 to build. The provider uses Terraform Plugin Framework v1.19.0 and
  Terraform Plugin Testing v1.14.1.

CLI matrix targeted by this repository's CI:

| CLI | Versions |
| --- | --- |
| Terraform | 1.10.0, 1.17.2 |
| OpenTofu | 1.12.0, 1.13.9 |

OpenTofu 1.14 is not stable and is not in the matrix. Other versions may work,
but this repository does not claim them.

## Installation

```hcl
terraform {
  required_providers {
    kubeutil = {
      source  = "geektr-cloud/kubeutil"
      version = "~> 0.1"
    }
  }
}
```

## Authentication

Provider configuration accepts the same settings as the Kubernetes provider,
merged in memory through client-go. No temporary kubeconfig is written.

| Attribute | Environment | Notes |
| --- | --- | --- |
| `host` | `KUBE_HOST` | API server URL |
| `username` | `KUBE_USER` | Not `KUBE_USERNAME` |
| `password` | `KUBE_PASSWORD` | Sensitive |
| `insecure` | `KUBE_INSECURE` | Skip TLS verification |
| `tls_server_name` | `KUBE_TLS_SERVER_NAME` | TLS SNI override |
| `client_certificate` | `KUBE_CLIENT_CERT_DATA` | PEM or base64 |
| `client_key` | `KUBE_CLIENT_KEY_DATA` | Sensitive |
| `cluster_ca_certificate` | `KUBE_CLUSTER_CA_CERT_DATA` | PEM or base64 |
| `config_path` | `KUBE_CONFIG_PATH` | Single kubeconfig file |
| `config_paths` | `KUBE_CONFIG_PATHS` | List of kubeconfig files |
| `config_context` | `KUBE_CTX` | Context name |
| `config_context_auth_info` | `KUBE_CTX_AUTH_INFO` | Auth info override |
| `config_context_cluster` | `KUBE_CTX_CLUSTER` | Cluster override |
| `token` | `KUBE_TOKEN` | Sensitive bearer token |
| `proxy_url` | `KUBE_PROXY_URL` | HTTP proxy for API and port-forward traffic |
| `default_namespace` | `KUBE_NAMESPACE` | Fallback namespace for tunnels |
| `exec { api_version, command, args, env }` | — | In-memory exec credential plugin; `env` is Sensitive |

Merge order, highest priority first:

1. Explicit provider attributes.
2. `KUBE_*` environment variables.
3. Kubeconfig files selected by `config_path` / `config_paths`, with context,
   auth info, and cluster overrides.
4. In-cluster service account, when running inside a pod.

The effective namespace is resolved as: surface `namespace`, then provider
`default_namespace`, then the credential context namespace (or in-cluster
namespace), then the literal `default`.

## Tunnel semantics and lifecycle

```hcl
ephemeral "kubeutil_tunnel" "api" {
  target    = "service/api"
  port      = "http" # a name, a number such as 8080, or ":8080"
  namespace = "app"
  timeout   = "60s"
}

# A provisioner is a legal consumer of ephemeral values: its configuration is
# never written to plan or state. Root-module outputs cannot consume them.
resource "terraform_data" "api_smoke_test" {
  provisioner "local-exec" {
    environment = {
      KUBEUTIL_ENDPOINT = ephemeral.kubeutil_tunnel.api.hostport
    }
    command = "curl -fsS \"http://$KUBEUTIL_ENDPOINT/\" > /dev/null"
  }
}
```

Inputs:

| Attribute | Required | Description |
| --- | --- | --- |
| `target` | yes | `type/name` or a bare pod name |
| `port` | yes | Number, port name, or either with an empty local prefix |
| `namespace` | no | Overrides the provider default |
| `timeout` | no | Duration for the whole acquisition, default `60s` |
| `healthcheck` | no | `healthcheck { type = "telnet" }` |

Outputs:

| Attribute | Description |
| --- | --- |
| `host` | Always `127.0.0.1` |
| `local_port` | OS-assigned TCP port |
| `hostport` | `127.0.0.1:<local_port>` |
| `namespace` | Effective namespace |
| `id` | Data source only: `<namespace>/<canonical-target>/<normalized-port>` |

Target rules:

- Built-in kinds only: `pod`, `service`, `deployment`, `replicaset`,
  `statefulset`, `daemonset`, `job`, `replicationcontroller`, plus the usual
  kubectl aliases (`po`, `svc`, `deploy`, `rs`, `sts`, `ds`, `rc`).
- A bare name means `pod/name`.
- Selector kinds resolve to a `Running` pod using kubectl-style selection.
  Services translate the requested Service port through the Service's
  `targetPort` to a container port; other target kinds resolve container ports
  directly.
- CRDs, label selectors, and `all` are not supported.
- No resolved pod name or translated target port is returned. A tunnel names a
  target port, not a pod incarnation.

Port rules:

- `5432` and `postgres` are valid.
- `:5432` and `:postgres` are valid.
- `8888:5432` is rejected. The provider always allocates the local port; it
  binds `127.0.0.1:0` and reports what the OS assigned. IPv4 loopback only.

Reconnect and drift:

- After acquisition, a keeper goroutine owns forwarding. It reconnects
  indefinitely with backoff from `1s` to `30s`.
- It re-resolves the target on every reconnect, so the tunnel may move to a
  different or replacement pod (pod drift). Even `pod/name` tracks the name, not
  the pod UID.
- The local port stays the same across reconnects, but the listener is down
  while reconnecting and existing forwarded connections are cut.
- Transport is WebSocket first, with SPDY fallback, so HTTP proxies that break
  SPDY upgrades still work.

Sharing:

- Tunnels are deduplicated per provider instance by effective namespace,
  canonical target, and normalized requested port. Identical requests share one
  listener and one local port.
- Ephemeral `Close` releases its reference. Data-source references are held
  until the provider process exits, because data sources have no close
  operation.

`timeout` covers the entire acquisition: target resolution, pod wait, API
upgrade, listener readiness, and healthcheck. It does not limit tunnel lifetime.

## Healthcheck

`healthcheck { type = "telnet" }` connects to the local listener without sending
application bytes, then watches for forwarding failure for up to `1s`, bounded
by the remaining `timeout`.

It is a **negative rejection check**:

- It can catch an immediate remote port refusal.
- A successful local handshake does not prove the remote port is open or the
  application is healthy.
- A delayed refusal can escape the one-second window and go unreported.
- It runs on every acquisition, including one that reuses a shared tunnel.

Without a `healthcheck` block, acquisition succeeds once the target resolves to a
`Running` pod, the API upgrade completes, and the listener is ready.

## Security and state

| Value | Display | Plan/state |
| --- | --- | --- |
| `password`, `token`, `client_key`, `exec.env` | Sensitive | Redacted in output only |
| `data.kubeutil_tunnel` outputs | Normal | Persisted |
| `ephemeral.kubeutil_tunnel` outputs | Normal | Not persisted |

**Sensitive is not encryption.** It only redacts CLI output. Provider
configuration is evaluated per command and is not written to state by these
surfaces, but plan files can contain configuration values. Protect plan
artifacts and use encrypted remote state where appropriate. The provider writes
no temporary kubeconfig.

Data source semantics:

- `data.kubeutil_tunnel` opens a real tunnel when read, including during
  planning. Reads perform API calls and can appear in audit logs.
- Every read may return a different `local_port`, so it can show up as a plan
  diff.
- The persisted endpoint is valid only for the command and provider process that
  read it. A later command, or applying a saved plan in another process, sees a
  dead port.
- State is not a tunnel resurrection mechanism. Removing and re-reading the data
  source opens a new tunnel, not the old one.

Ephemeral semantics:

- `ephemeral.kubeutil_tunnel` values never persist and are usable only in
  ephemeral contexts, such as a provisioner or provider configuration, a
  write-only argument, or another ephemeral resource. Root-module outputs cannot
  consume them, even with `ephemeral = true`.
- An ordinary resource argument cannot consume an ephemeral value.

## RBAC

The provider needs read access to the target and to pods, plus the port-forward
subresource:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kubeutil-tunnel
  namespace: app
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get", "list"]
  - apiGroups: ["apps"]
    resources: ["deployments", "replicasets", "statefulsets", "daemonsets"]
    verbs: ["get", "list"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["replicationcontrollers"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["create"]
```

Only grant the target kinds you use. Listing and watching pods is required for
selector-based targets; `pod/name` needs `get` on the pod and `create` on
`pods/portforward`.

## Examples

Runnable examples live under [examples](examples):

- [provider](examples/provider/provider.tf)
- [data source](examples/data-sources/kubeutil_tunnel/data-source.tf)
- [ephemeral resource](examples/ephemeral-resources/kubeutil_tunnel/ephemeral-resource.tf)

A data source result may feed ordinary persistent arguments, but only with the
process-lifetime warning above:

```hcl
data "kubeutil_tunnel" "api" {
  target    = "deployment/api"
  port      = "http"
  namespace = "app"
}

output "api_endpoint_during_this_run" {
  value = data.kubeutil_tunnel.api.hostport
}
```

The ephemeral surface must stay in an ephemeral context. A provisioner is the
simplest one, and `terraform_data` needs no extra provider:

```hcl
ephemeral "kubeutil_tunnel" "api" {
  target = "service/api"
  port   = 8080
}

resource "terraform_data" "api_smoke_test" {
  provisioner "local-exec" {
    environment = {
      KUBEUTIL_ENDPOINT = ephemeral.kubeutil_tunnel.api.hostport
    }
    command = "curl -fsS \"http://$KUBEUTIL_ENDPOINT/\" > /dev/null"
  }
}
```

~> A root-module output cannot consume an ephemeral value, even with
`ephemeral = true`. Use a provisioner, a provider configuration, a write-only
argument where the CLI supports one, or another ephemeral resource instead.

## Development

```sh
gofmt -l .
go vet ./...
go build ./...
go test -race ./...
go mod verify
go mod tidy -diff
golangci-lint run
govulncheck ./...
```

Acceptance tests are hermetic and run only with `TF_ACC`:

```sh
TF_ACC=1 TF_ACC_TERRAFORM_PATH="$(command -v terraform)" go test ./internal/provider -run TestAcc -skip TestAccKind -v
```

Kind-backed acceptance tests need a cluster and set `KUBEUTIL_KIND=1`. First
create the cluster and apply the `echo` Deployment/Service fixture shown in the
`deploy test workload` step of `.github/workflows/test.yml`, then run:

```sh
kind create cluster --name kubeutil
TF_ACC=1 KUBEUTIL_KIND=1 TF_ACC_TERRAFORM_PATH="$(command -v terraform)" \
  go test ./internal/provider -run TestAccKind -v
```

CI is configured to run the unit/build checks, the hermetic acceptance matrix,
and a kind job for Terraform 1.10.0 and OpenTofu 1.12.0. See
[docs/design.md](docs/design.md) for the design contract and
[docs/adr](docs/adr) for decisions.

Generate provider documentation after schema changes:

```sh
go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@v0.25.0 generate --provider-name kubeutil
```

## License

MIT. See [LICENSE](LICENSE).
