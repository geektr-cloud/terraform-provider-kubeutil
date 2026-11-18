terraform {
  required_providers {
    kubeutil = {
      source = "geektr-cloud/kubeutil"
    }
  }
}

provider "kubeutil" {
  config_path       = "~/.kube/config"
  config_context    = "staging"
  default_namespace = "default"
}
