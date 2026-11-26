resource "kubernetes_secret_v1" "env" {
  for_each = local.enabled

  metadata {
    name      = local.names[each.key]
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = each.key })
  }

  data = local.env[each.key]
}

resource "kubernetes_config_map_v1" "files" {
  for_each = { for name, a in local.enabled : name => a if length(a.files) > 0 }

  metadata {
    name      = local.names[each.key]
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = each.key })
  }

  data = each.value.files
}

resource "kubernetes_config_map_v1" "scripts" {
  for_each = { for name, a in local.enabled : name => a if name == "router" && length(var.router.scripts) > 0 }

  metadata {
    name      = "${local.names[each.key]}-scripts"
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = each.key })
  }

  data = var.router.scripts
}

resource "kubernetes_persistent_volume_claim_v1" "state" {
  for_each = { for name, a in local.enabled : name => a if a.state_size > 0 }

  metadata {
    name      = local.names[each.key]
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = each.key })
  }

  spec {
    access_modes = ["ReadWriteOnce"]
    resources {
      requests = {
        storage = "${each.value.state_size}Gi"
      }
    }
  }

  # Storage classes with WaitForFirstConsumer bind the claim only when the pod starts.
  wait_until_bound = false
}

resource "kubernetes_persistent_volume_claim_v1" "site" {
  count = local.statuspage_site ? 1 : 0

  metadata {
    name      = "${local.names.statuspage}-site"
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = "statuspage" })
  }

  spec {
    access_modes = ["ReadWriteOnce"]
    resources {
      requests = {
        storage = "${coalesce(var.statuspage.output_size_gi, 1)}Gi"
      }
    }
  }

  wait_until_bound = false

  lifecycle {
    precondition {
      condition = (startswith(local.statuspage_output_dir, "/")
      && !contains(["/", "/tmp", local.state_dir, local.config_dir], local.statuspage_output_dir))
      error_message = "statuspage output_dir must be an absolute path other than /, /tmp, ${local.state_dir}, and ${local.config_dir}."
    }
  }
}

resource "kubernetes_deployment_v1" "main" {
  for_each = local.enabled

  metadata {
    name      = local.names[each.key]
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = each.key })
  }

  spec {
    # Each automation drops repeated events in memory, so a second replica would act on an event twice.
    replicas = 1

    strategy {
      type = "Recreate"
    }

    selector {
      match_labels = {
        "app.kubernetes.io/name"     = each.key
        "app.kubernetes.io/instance" = local.name_prefix
      }
    }

    template {
      metadata {
        labels = merge(local.labels, { "app.kubernetes.io/name" = each.key })
        annotations = {
          # Restarts the pod when its settings change. The files and the env are read only at start.
          "massdriver.cloud/config-hash" = sha256(jsonencode([
            local.env[each.key], each.value.files, each.key == "router" ? var.router.scripts : {}
          ]))
        }
      }

      spec {
        automount_service_account_token = false

        security_context {
          run_as_non_root = true
          run_as_user     = 65532
          run_as_group    = 65532
          fs_group        = 65532
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name              = each.key
          image             = each.value.image
          image_pull_policy = "Always"
          args              = ["run", each.key]

          port {
            name           = "http"
            container_port = 8080
          }

          env_from {
            secret_ref {
              name = kubernetes_secret_v1.env[each.key].metadata[0].name
            }
          }

          resources {
            requests = {
              cpu    = each.key == "bundle-scanner" ? "250m" : "25m"
              memory = each.key == "bundle-scanner" ? "512Mi" : "64Mi"
            }
            limits = {
              cpu    = each.key == "bundle-scanner" ? "2" : "500m"
              memory = each.key == "bundle-scanner" ? "2Gi" : "256Mi"
            }
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            period_seconds = 30
          }

          readiness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            period_seconds = 10
          }

          security_context {
            allow_privilege_escalation = false
            # Trivy and Checkov write caches and temporary files outside /tmp.
            read_only_root_filesystem = each.key != "bundle-scanner"
            capabilities {
              drop = ["ALL"]
            }
          }

          volume_mount {
            name       = "tmp"
            mount_path = "/tmp"
          }

          dynamic "volume_mount" {
            for_each = length(each.value.files) > 0 ? [1] : []
            content {
              name       = "files"
              mount_path = local.config_dir
              read_only  = true
            }
          }

          dynamic "volume_mount" {
            for_each = each.key == "router" && length(var.router.scripts) > 0 ? [1] : []
            content {
              name       = "scripts"
              mount_path = local.scripts_dir
              read_only  = true
            }
          }

          dynamic "volume_mount" {
            for_each = each.value.state_size > 0 ? [1] : []
            content {
              name       = "state"
              mount_path = local.state_dir
            }
          }

          dynamic "volume_mount" {
            for_each = each.key == "statuspage" && local.statuspage_site ? [1] : []
            content {
              name       = "site"
              mount_path = local.statuspage_output_dir
            }
          }
        }

        volume {
          name = "tmp"
          empty_dir {}
        }

        dynamic "volume" {
          for_each = length(each.value.files) > 0 ? [1] : []
          content {
            name = "files"
            config_map {
              name = kubernetes_config_map_v1.files[each.key].metadata[0].name
            }
          }
        }

        dynamic "volume" {
          for_each = each.key == "router" && length(var.router.scripts) > 0 ? [1] : []
          content {
            name = "scripts"
            config_map {
              name         = kubernetes_config_map_v1.scripts[each.key].metadata[0].name
              default_mode = "0755"
            }
          }
        }

        dynamic "volume" {
          for_each = each.value.state_size > 0 ? [1] : []
          content {
            name = "state"
            persistent_volume_claim {
              claim_name = kubernetes_persistent_volume_claim_v1.state[each.key].metadata[0].name
            }
          }
        }

        dynamic "volume" {
          for_each = each.key == "statuspage" && local.statuspage_site ? [1] : []
          content {
            name = "site"
            persistent_volume_claim {
              claim_name = kubernetes_persistent_volume_claim_v1.site[0].metadata[0].name
            }
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "main" {
  for_each = local.enabled

  metadata {
    name      = local.names[each.key]
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = each.key })
  }

  spec {
    selector = {
      "app.kubernetes.io/name"     = each.key
      "app.kubernetes.io/instance" = local.name_prefix
    }
    port {
      name        = "http"
      port        = 80
      target_port = "http"
    }
  }
}

resource "kubernetes_ingress_v1" "main" {
  for_each = local.enabled

  metadata {
    name      = local.names[each.key]
    namespace = local.namespace
    labels    = merge(local.labels, { "app.kubernetes.io/name" = each.key })
    annotations = local.cluster_issuer != "" ? {
      "cert-manager.io/cluster-issuer" = local.cluster_issuer
    } : {}
  }

  spec {
    ingress_class_name = local.ingress_class

    # Every path goes to the automation: Massdriver events, Slack interactions, and the status site.
    rule {
      host = each.value.host
      http {
        path {
          path      = "/"
          path_type = "Prefix"
          backend {
            service {
              name = kubernetes_service_v1.main[each.key].metadata[0].name
              port {
                name = "http"
              }
            }
          }
        }
      }
    }

    dynamic "tls" {
      for_each = local.cluster_issuer != "" || local.tls_secret_name != "" ? [1] : []
      content {
        hosts       = [each.value.host]
        secret_name = local.cluster_issuer != "" ? "${local.names[each.key]}-tls" : local.tls_secret_name
      }
    }
  }
}
