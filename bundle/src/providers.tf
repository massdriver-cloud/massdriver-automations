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
  host                   = var.kubernetes_cluster.cluster.server
  cluster_ca_certificate = local.cluster_ca
  tls_server_name        = try(var.kubernetes_cluster.cluster["tls-server-name"], null)
  insecure               = try(var.kubernetes_cluster.cluster["insecure-skip-tls-verify"], null)
  proxy_url              = try(var.kubernetes_cluster.cluster["proxy-url"], null)
  token                  = try(var.kubernetes_cluster.user.token, null)
  client_certificate     = local.client_certificate
  client_key             = local.client_key

  dynamic "exec" {
    for_each = try(var.kubernetes_cluster.user.exec, null) == null ? [] : [var.kubernetes_cluster.user.exec]
    content {
      api_version = exec.value.apiVersion
      command     = exec.value.command
      args        = try(exec.value.args, null)
      env         = { for e in try(coalesce(exec.value.env), []) : e.name => e.value }
    }
  }
}

# A deployment's own token cannot create event destinations, so the provider uses the service account.
provider "massdriver" {
  url             = var.service_account.url
  organization_id = var.service_account.organization_id
  api_key         = var.service_account.api_key
}

locals {
  cluster_ca         = try(base64decode(var.kubernetes_cluster.cluster["certificate-authority-data"]), null)
  client_certificate = try(base64decode(var.kubernetes_cluster.user["client-certificate-data"]), null)
  client_key         = try(base64decode(var.kubernetes_cluster.user["client-key-data"]), null)
}
