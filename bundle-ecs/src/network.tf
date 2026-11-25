resource "aws_security_group" "tasks" {
  count = length(local.enabled) > 0 ? 1 : 0

  name_prefix = "${trimsuffix(substr(local.name_prefix, 0, 40), "-")}-tasks-"
  description = "Massdriver automations of ${local.name_prefix}"
  vpc_id      = var.ecs_cluster.vpc_id

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "tasks_from_load_balancer" {
  count = length(local.enabled) > 0 ? 1 : 0

  security_group_id            = aws_security_group.tasks[0].id
  description                  = "HTTP from the load balancer"
  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
  referenced_security_group_id = var.ecs_cluster.load_balancer.security_group_id
}

# Automations call the Massdriver API, Slack, GitHub, and the other services that their settings name.
resource "aws_vpc_security_group_egress_rule" "tasks_ipv4" {
  count = length(local.enabled) > 0 ? 1 : 0

  security_group_id = aws_security_group.tasks[0].id
  description       = "All outbound traffic"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_lb_target_group" "main" {
  for_each = local.enabled

  name                 = local.target_group_names[each.key]
  vpc_id               = var.ecs_cluster.vpc_id
  target_type          = "ip"
  protocol             = "HTTP"
  port                 = 8080
  deregistration_delay = 10

  health_check {
    path                = "/healthz"
    matcher             = "200"
    interval            = 15
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }
}

# Every path of the host goes to the automation: Massdriver events, Slack interactions, and the status site.
resource "aws_lb_listener_rule" "main" {
  for_each = local.enabled

  listener_arn = var.ecs_cluster.load_balancer.https_listener_arn

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.main[each.key].arn
  }

  condition {
    host_header {
      values = [each.value.host]
    }
  }
}

resource "aws_route53_record" "main" {
  for_each = local.dns_hosts

  zone_id = local.zone_id
  name    = each.value
  type    = "CNAME"
  ttl     = 300
  records = [var.ecs_cluster.load_balancer.dns_name]
}
