data "aws_subnet" "private" {
  for_each = length(local.stateful) > 0 ? toset(var.ecs_cluster.private_subnet_ids) : toset([])

  id = each.value
}

locals {
  # EFS allows one mount target in each Availability Zone.
  mount_subnets = {
    for az, ids in { for id, s in data.aws_subnet.private : s.availability_zone => id... } : az => sort(ids)[0]
  }
}

resource "aws_efs_file_system" "state" {
  count = length(local.stateful) > 0 ? 1 : 0

  encrypted  = true
  kms_key_id = aws_kms_key.main[0].arn

  tags = {
    Name = "${local.name_prefix}-state"
  }
}

resource "aws_security_group" "efs" {
  count = length(local.stateful) > 0 ? 1 : 0

  name_prefix = "${trimsuffix(substr(local.name_prefix, 0, 40), "-")}-efs-"
  description = "State of the Massdriver automations of ${local.name_prefix}"
  vpc_id      = var.ecs_cluster.vpc_id

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "efs_from_tasks" {
  count = length(local.stateful) > 0 ? 1 : 0

  security_group_id            = aws_security_group.efs[0].id
  description                  = "NFS from the automations"
  ip_protocol                  = "tcp"
  from_port                    = 2049
  to_port                      = 2049
  referenced_security_group_id = aws_security_group.tasks[0].id
}

resource "aws_efs_mount_target" "state" {
  for_each = local.mount_subnets

  file_system_id  = aws_efs_file_system.state[0].id
  subnet_id       = each.value
  security_groups = [aws_security_group.efs[0].id]
}

resource "aws_efs_access_point" "state" {
  for_each = local.stateful

  file_system_id = aws_efs_file_system.state[0].id

  posix_user {
    uid = 65532
    gid = 65532
  }

  root_directory {
    path = "/${each.key}"
    creation_info {
      owner_uid   = 65532
      owner_gid   = 65532
      permissions = "0750"
    }
  }

  tags = {
    Name = local.names[each.key]
  }
}
