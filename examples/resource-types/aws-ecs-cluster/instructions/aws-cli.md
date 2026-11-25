# AWS CLI

You need the AWS CLI v2 and credentials that can read ECS, EC2, Elastic Load Balancing, and Route 53. Set the
cluster name, the Region, and the name of the load balancer:

```bash
CLUSTER=platform
LOAD_BALANCER=platform
export AWS_REGION=us-east-1
```

## Cluster

```bash
# cluster.arn
aws ecs describe-clusters --clusters "$CLUSTER" --query 'clusters[0].clusterArn' --output text
```

`cluster.name` is `$CLUSTER`, and `region` is `$AWS_REGION`.

## Load balancer

The load balancer must be internet-facing, because Massdriver and Slack send requests to the services from the
internet.

```bash
LB_ARN=$(aws elbv2 describe-load-balancers --names "$LOAD_BALANCER" \
  --query 'LoadBalancers[0].LoadBalancerArn' --output text)

# vpc_id
aws elbv2 describe-load-balancers --load-balancer-arns "$LB_ARN" --query 'LoadBalancers[0].VpcId' --output text

# load_balancer.dns_name
aws elbv2 describe-load-balancers --load-balancer-arns "$LB_ARN" --query 'LoadBalancers[0].DNSName' --output text

# load_balancer.security_group_id
aws elbv2 describe-load-balancers --load-balancer-arns "$LB_ARN" \
  --query 'LoadBalancers[0].SecurityGroups[0]' --output text

# load_balancer.https_listener_arn
aws elbv2 describe-listeners --load-balancer-arn "$LB_ARN" \
  --query 'Listeners[?Protocol==`HTTPS`].ListenerArn | [0]' --output text
```

The security group of the load balancer must allow outbound traffic to the VPC, so the load balancer can reach the
tasks.

## Private subnets

List the subnets of the VPC with their Availability Zones. Pick one subnet in each zone whose route table sends
`0.0.0.0/0` to a NAT gateway:

```bash
VPC_ID=vpc-0a1b2c3d4e5f67890

aws ec2 describe-subnets --filters "Name=vpc-id,Values=$VPC_ID" \
  --query 'Subnets[?MapPublicIpOnLaunch==`false`].[SubnetId,AvailabilityZone]' --output table
```

## DNS (optional)

```bash
DOMAIN=acme.com

# dns.zone_id
aws route53 list-hosted-zones-by-name --dns-name "$DOMAIN" \
  --query 'HostedZones[?Config.PrivateZone==`false`] | [0].Id' --output text | sed 's|/hostedzone/||'
```

`dns.base_domain` is `$DOMAIN`.
