locals {
  log_group_prefix = "/ecs/${trimsuffix(substr(local.name_prefix, 0, 41), "-")}-"

  write_files = <<-PY
    import json, os
    os.chmod("/tmp", 0o1777)
    for f in json.loads(os.environ["FILES"]):
        os.chmod(os.path.dirname(f["path"]), 0o755)
        with open(f["path"], "w") as out:
            out.write(f["content"])
        os.chmod(f["path"], int(f["mode"], 8))
  PY
}

resource "aws_kms_key" "main" {
  count = length(local.enabled) > 0 ? 1 : 0

  description             = "Secrets, logs, and state of ${local.name_prefix}"
  deletion_window_in_days = 7
  enable_key_rotation     = true

  # In a key policy, resource "*" is this key.
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "Account"
        Effect    = "Allow"
        Principal = { AWS = "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root" }
        Action    = "kms:*"
        Resource  = "*"
      },
      {
        # CloudWatch Logs encrypts with the key itself, so the key policy must allow the service.
        Sid       = "CloudWatchLogs"
        Effect    = "Allow"
        Principal = { Service = "logs.${var.ecs_cluster.region}.amazonaws.com" }
        Action    = ["kms:Encrypt*", "kms:Decrypt*", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:Describe*"]
        Resource  = "*"
        Condition = {
          ArnLike = {
            "kms:EncryptionContext:aws:logs:arn" = "arn:${data.aws_partition.current.partition}:logs:${var.ecs_cluster.region}:${data.aws_caller_identity.current.account_id}:log-group:${local.log_group_prefix}*"
          }
        }
      },
    ]
  })
}

resource "aws_kms_alias" "main" {
  count = length(local.enabled) > 0 ? 1 : 0

  name          = "alias/${local.name_prefix}"
  target_key_id = aws_kms_key.main[0].key_id
}

resource "aws_cloudwatch_log_group" "main" {
  for_each = local.enabled

  name              = "${local.log_group_prefix}${each.key}"
  retention_in_days = var.logs.retention_days
  kms_key_id        = aws_kms_key.main[0].arn
}

resource "aws_secretsmanager_secret" "main" {
  for_each = local.enabled

  name_prefix             = "${local.names[each.key]}-"
  description             = "Secret environment variables of ${each.key}"
  kms_key_id              = aws_kms_key.main[0].arn
  recovery_window_in_days = 7
}

resource "aws_secretsmanager_secret_version" "main" {
  for_each = local.enabled

  secret_id     = aws_secretsmanager_secret.main[each.key].id
  secret_string = jsonencode(local.secret_env[each.key])
}

resource "aws_ecs_task_definition" "main" {
  for_each = local.enabled

  family                   = local.names[each.key]
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = each.key == "bundle-scanner" ? 1024 : 256
  memory                   = each.key == "bundle-scanner" ? 2048 : 512
  execution_role_arn       = aws_iam_role.execution[each.key].arn
  task_role_arn            = try(aws_iam_role.task[each.key].arn, null)

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  volume {
    name = "tmp"
  }

  volume {
    name = "config"
  }

  volume {
    name = "scripts"
  }

  dynamic "volume" {
    for_each = each.value.state ? [1] : []
    content {
      name = "state"
      efs_volume_configuration {
        file_system_id     = aws_efs_file_system.state[0].id
        transit_encryption = "ENABLED"
        authorization_config {
          access_point_id = aws_efs_access_point.state[each.key].id
          iam             = "ENABLED"
        }
      }
    }
  }

  dynamic "volume" {
    for_each = each.key == "statuspage" && local.statuspage_site ? [1] : []
    content {
      name = "site"
      efs_volume_configuration {
        file_system_id     = aws_efs_file_system.state[0].id
        transit_encryption = "ENABLED"
        authorization_config {
          access_point_id = aws_efs_access_point.site[0].id
          iam             = "ENABLED"
        }
      }
    }
  }

  container_definitions = jsonencode([
    {
      name                   = "files"
      image                  = "${local.image}-scripts"
      essential              = false
      user                   = "0"
      readonlyRootFilesystem = true
      entryPoint             = ["python3", "-c"]
      command                = [local.write_files]
      environment            = [{ name = "FILES", value = jsonencode(local.files[each.key]) }]
      mountPoints = [
        { sourceVolume = "tmp", containerPath = "/tmp", readOnly = false },
        { sourceVolume = "config", containerPath = local.config_dir, readOnly = false },
        { sourceVolume = "scripts", containerPath = local.scripts_dir, readOnly = false },
      ]
      linuxParameters = {
        capabilities = { drop = ["ALL"] }
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.main[each.key].name
          awslogs-region        = var.ecs_cluster.region
          awslogs-stream-prefix = "files"
        }
      }
    },
    {
      name      = each.key
      image     = each.value.image
      essential = true
      command   = ["run", each.key]
      user      = "65532:65532"
      # Trivy and Checkov write caches and temporary files outside /tmp.
      readonlyRootFilesystem = each.key != "bundle-scanner"
      portMappings           = [{ name = "http", containerPort = 8080, protocol = "tcp" }]
      environment = [
        for k in sort(keys(local.settings[each.key])) : { name = k, value = local.settings[each.key][k] }
      ]
      # The version id makes a new task definition when a secret changes, so the service restarts with it.
      secrets = [
        for k in sort(keys(local.secret_env[each.key])) : {
          name      = k
          valueFrom = "${aws_secretsmanager_secret.main[each.key].arn}:${k}::${aws_secretsmanager_secret_version.main[each.key].version_id}"
        }
      ]
      dependsOn = [{ containerName = "files", condition = "SUCCESS" }]
      mountPoints = concat(
        [
          { sourceVolume = "tmp", containerPath = "/tmp", readOnly = false },
          { sourceVolume = "config", containerPath = local.config_dir, readOnly = true },
          { sourceVolume = "scripts", containerPath = local.scripts_dir, readOnly = true },
        ],
        each.value.state ? [{ sourceVolume = "state", containerPath = local.state_dir, readOnly = false }] : [],
        each.key == "statuspage" && local.statuspage_site ? [
          { sourceVolume = "site", containerPath = local.statuspage_output_dir, readOnly = false }
        ] : [],
      )
      linuxParameters = {
        capabilities       = { drop = ["ALL"] }
        initProcessEnabled = true
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.main[each.key].name
          awslogs-region        = var.ecs_cluster.region
          awslogs-stream-prefix = "automation"
        }
      }
    },
  ])
}

resource "aws_ecs_service" "main" {
  for_each = local.enabled

  name             = local.names[each.key]
  cluster          = var.ecs_cluster.cluster.arn
  task_definition  = aws_ecs_task_definition.main[each.key].arn
  launch_type      = "FARGATE"
  platform_version = "LATEST"

  # Each automation drops repeated events in memory, so a second task would act on an event twice.
  desired_count                      = 1
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100
  health_check_grace_period_seconds  = 60
  wait_for_steady_state              = true

  enable_ecs_managed_tags = true
  propagate_tags          = "SERVICE"

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = var.ecs_cluster.private_subnet_ids
    security_groups  = [aws_security_group.tasks[0].id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.main[each.key].arn
    container_name   = each.key
    container_port   = 8080
  }

  # A target group must belong to a listener before a service uses it, and tasks mount EFS through a mount target.
  depends_on = [aws_lb_listener_rule.main, aws_efs_mount_target.state]
}
