locals {
  signature = var.config.signature == null ? "" : trimspace(var.config.signature)
  sign_off  = var.config.sign_off != false

  # Without a signature key the automation picks a built-in sign-off, and a null signature turns sign-offs off.
  config = merge(
    { for k, v in var.config : k => v if !contains(["match", "signature", "sign_off"], k) },
    # Attribute values with commas become lists, which match any of their values.
    { match = { for k, v in coalesce(var.config.match, {}) : k => [for x in split(",", v) : trimspace(x)] } },
    { for k, v in { signature = local.signature } : k => v if local.sign_off && local.signature != "" },
    { for k, v in { signature = null } : k => v if !local.sign_off },
  )

  automation = {
    name  = "chaos-monkey"
    image = local.image
    files = { (local.config_file) = yamlencode(local.config) }
    env   = local.massdriver_env
    secrets = merge(local.massdriver_secrets, local.slack_secrets, {
      CHAOS_MONKEY_RUN_TOKEN = lookup(local.secrets, "CHAOS_MONKEY_RUN_TOKEN", "")
    })
    event_types = ["deployment.completed", "deployment.failed", "deployment.aborted"]
    attributes  = {}
    condition   = "(.data.entity.message // \"\") | startswith(\"chaos-monkey: \")"
  }
}
