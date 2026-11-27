locals {
  # The same default as the automation, so the destination sends what it acts on.
  repo_attribute = try(coalesce(var.config.github.repo_attribute), "github_repo")

  automation = {
    name  = "github-deployments"
    image = local.image
    files = { (local.config_file) = yamlencode(var.config) }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, {
      GITHUB_TOKEN = lookup(local.secrets, "GITHUB_TOKEN", "")
    })
    event_types = ["deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted"]
    condition   = ".data.metadata.deployment_action != \"PLAN\" and (.data.attributes[${jsonencode(local.repo_attribute)}] // \"\") != \"\""
  }
}
