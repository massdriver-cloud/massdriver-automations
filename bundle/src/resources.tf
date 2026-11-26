resource "massdriver_resource" "status_page" {
  count = var.statuspage.enabled ? 1 : 0

  field = "status_page"
  name  = "${local.name_prefix} status page"
  resource = jsonencode({
    url             = "https://${local.statuspage_host}"
    status_json_url = "https://${local.statuspage_host}/api/status.json"
    pages = [
      for p in local.statuspage_pages : {
        title     = p.title
        slug      = p.slug
        url       = "https://${local.statuspage_host}/${p.slug}/"
        feed_url  = "https://${local.statuspage_host}/${p.slug}/feed.atom"
        badge_url = "https://${local.statuspage_host}/${p.slug}/badge.svg"
      }
    ]
  })
}
