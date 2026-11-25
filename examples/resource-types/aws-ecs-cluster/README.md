# aws-ecs-cluster (example)

An example resource type for an Amazon ECS cluster that runs Fargate services behind a shared Application Load
Balancer. It is not an official Massdriver resource type. Copy it to your own resource types repository, change the
fields to match how your organization runs ECS, and publish it:

```bash
mass resource-type publish massdriver.yaml
```

The [`massdriver-automations-ecs`](../../../bundle-ecs) bundle depends on this type. If you rename the type or its
fields, change the bundle's `dependencies` and `src/` to match.

## Fields

| Field | Required | |
|---|---|---|
| `cluster.name`, `cluster.arn` | Yes | The ECS cluster. |
| `region` | Yes | The Region of the cluster, the VPC, and the load balancer. |
| `vpc_id` | Yes | The VPC of the subnets and the load balancer. |
| `private_subnet_ids` | Yes | Subnets for tasks, one in each Availability Zone, with a route through a NAT gateway. |
| `load_balancer.https_listener_arn` | Yes | The HTTPS listener that services add host-header rules to. |
| `load_balancer.security_group_id` | Yes | The security group of the load balancer. Services allow traffic from it. |
| `load_balancer.dns_name` | Yes | The DNS name of the load balancer, for CNAME records. |
| `dns.zone_id`, `dns.base_domain` | No | A public Route 53 hosted zone for service hosts. |

The bundle that owns the cluster creates the resource with `massdriver_resource`, or you create it by hand from the
values in [instructions/aws-cli.md](instructions/aws-cli.md).

