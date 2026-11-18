# The data source opens a real tunnel during plan and persists the endpoint.
# That endpoint is only valid for the command that read it. Prefer
# ephemeral.kubeutil_tunnel whenever the consumer can accept an ephemeral value,
# such as a provisioner.
data "kubeutil_tunnel" "postgres" {
  target    = "service/postgres"
  port      = "postgres" # also accepts 5432 or ":5432"
  namespace = "app"

  timeout = "60s"

  healthcheck {
    type = "telnet"
  }
}

output "postgres_endpoint" {
  value       = data.kubeutil_tunnel.postgres.hostport
  description = "Loopback endpoint; dead after this command exits."
}
