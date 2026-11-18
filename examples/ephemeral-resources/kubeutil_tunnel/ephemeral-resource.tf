# Ephemeral values cannot be returned from a root-module output, even with
# ephemeral = true. A provisioner is a legal consumer because its configuration
# is never persisted in plan or state.
ephemeral "kubeutil_tunnel" "api" {
  target    = "service/api"
  port      = "http"
  namespace = "app"
}

resource "terraform_data" "api_smoke_test" {
  provisioner "local-exec" {
    environment = {
      KUBEUTIL_ENDPOINT = ephemeral.kubeutil_tunnel.api.hostport
    }
    command = "curl -fsS \"http://$KUBEUTIL_ENDPOINT/\" > /dev/null"
  }
}
