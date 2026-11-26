# website (example)

An example resource type for a web page that people open: one `url`. It is not an official Massdriver resource
type. Copy it to your own resource types repository, change it if you need to, and publish it:

```bash
mass resource-type create website
mass resource-type publish .
```

[massdriver-statuspage](../../../bundles/massdriver-statuspage) and
[massdriver-cost-explorer](../../../bundles/massdriver-cost-explorer), and their `-ecs` versions, produce a
`website` resource with the URL of the status page or the cost dashboard.
