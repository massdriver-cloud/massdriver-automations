terraform {
  required_version = ">= 1.10"
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.0"
    }
    massdriver = {
      source = "massdriver-cloud/massdriver"
      # 2.7.0 adds signing_secret on massdriver_event_destination.
      version = "~> 2.7"
    }
  }
}

provider "kubernetes" {
  host                   = var.kubernetes_cluster.authentication.cluster.server
  cluster_ca_certificate = base64decode(var.kubernetes_cluster.authentication.cluster["certificate-authority-data"])
  token                  = var.kubernetes_cluster.authentication.user.token
}

# The deployment token cannot create event destinations, so the provider uses the service account token. The
# organization and API URL come from the MASSDRIVER_* variables of the deployment.
provider "massdriver" {
  api_key = lookup(local.secrets, "MASSDRIVER_API_KEY", null)
}
