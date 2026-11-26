# status-page (example)

An example resource type for a public status page. It is not an official Massdriver resource type. Copy it to your
own resource types repository, change it if you need to, and publish it:

```bash
mass resource-type create status-page
mass resource-type publish massdriver.yaml
```

Both bundles produce a `status_page` resource of this type when statuspage runs. If you rename the type or its
fields, change the bundles' `resources` and `src/resources.tf` to match.

## Fields

| Field | |
|---|---|
| `url` | The status site, such as `https://status.acme.com`. |
| `status_json_url` | `<url>/api/status.json`: every page's status, components, uptime, incidents, and maintenance. |
| `pages[].title`, `pages[].slug` | Each page, in the order of the statuspage config. |
| `pages[].url` | `<url>/<slug>/`. |
| `pages[].feed_url` | `<url>/<slug>/feed.atom`, an Atom feed of the page's incidents. |
| `pages[].badge_url` | `<url>/<slug>/badge.svg`, an SVG badge of the page's status. |
