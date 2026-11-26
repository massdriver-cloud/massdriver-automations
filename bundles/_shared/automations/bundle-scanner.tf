locals {
  automation = {
    name        = "bundle-scanner"
    image       = "${local.image}-scanner"
    files       = { (local.config_file) = yamlencode(var.config) }
    env         = local.massdriver_env
    secrets     = merge(local.massdriver_secrets, local.slack_secrets)
    event_types = ["repo.published"]
    attributes  = coalesce(var.attributes, {})
    condition   = ".data.entity.artifact_type == \"application/vnd.massdriver.bundle.v1+json\""
    # Trivy and Checkov need more memory, and write caches outside /tmp.
    large = true
  }
}
