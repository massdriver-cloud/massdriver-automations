# aws-ecs-cluster (example)

An example resource type for an Amazon ECS cluster that runs Fargate services behind a shared Application Load
Balancer. It is not an official Massdriver resource type. Copy it to your own resource types repository, change the
fields to match how your organization runs ECS, and publish it:

```bash
mass resource-type publish .
```

The `-ecs` bundles in [bundles](../../../bundles) depend on this type. If you rename the type or its
fields, change the bundle's `dependencies` and `src/` to match.

## Fields

| Field | Required | |
|---|---|---|
| `name`, `arn` | Yes | The ECS cluster. |
| `region` | Yes | The Region of the cluster, the VPC, and the load balancer. |
| `network.id` | Yes | The VPC of the subnets and the load balancer. |
| `network.private_subnet_ids` | Yes | Subnets for tasks, one in each Availability Zone. |
| `network.internet_egress` | Yes | Whether the private subnets can reach the internet, through a NAT gateway. |
| `load_balancer.arn`, `load_balancer.dns_name`, `load_balancer.zone_id` | Yes | The load balancer, its DNS name for CNAME records, and its own hosted zone for alias records. |
| `load_balancer.listener_arn` | Yes | The listener that services add host-header rules to. HTTPS when the cluster has a DNS zone. |
| `load_balancer.protocol` | No | `HTTP` or `HTTPS`, the protocol of the listener. |
| `load_balancer.security_group_id` | Yes | The security group of the load balancer. Services allow traffic from it. |
| `zone.zone_id`, `zone.domain` | No | A public Route 53 hosted zone for service hosts. Present when the listener is HTTPS. |

The bundle that owns the cluster creates the resource with `massdriver_resource`, or you create it by hand from the
values in [instructions/aws-cli.md](instructions/aws-cli.md).

