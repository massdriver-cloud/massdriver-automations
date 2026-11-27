locals {
  ecr_image = try(regex("^(?P<account>[0-9]{12})\\.dkr\\.ecr\\.(?P<region>[a-z0-9-]+)\\.amazonaws\\.com(?:\\.cn)?/(?P<repository>.+)$", var.image.repository), null)

  task_roles = {
    for name, a in local.enabled : name => a
    if a.state || a.s3_write != null
  }
}

data "aws_iam_policy_document" "assume_task" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

resource "aws_iam_role" "execution" {
  for_each = local.enabled

  name               = "${local.role_names[each.key]}-exec"
  assume_role_policy = data.aws_iam_policy_document.assume_task.json
}

data "aws_iam_policy_document" "execution" {
  for_each = local.enabled

  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.main[each.key].arn}:*"]
  }

  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.main[each.key].arn]
  }

  statement {
    actions   = ["kms:Decrypt"]
    resources = [aws_kms_key.main[0].arn]
  }

  dynamic "statement" {
    for_each = local.ecr_image == null ? [] : [1]
    content {
      actions   = ["ecr:GetAuthorizationToken"]
      resources = ["*"]
    }
  }

  dynamic "statement" {
    for_each = local.ecr_image == null ? [] : [local.ecr_image]
    content {
      actions   = ["ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
      resources = ["arn:${data.aws_partition.current.partition}:ecr:${statement.value.region}:${statement.value.account}:repository/${statement.value.repository}"]
    }
  }
}

resource "aws_iam_role_policy" "execution" {
  for_each = local.enabled

  name   = "execution"
  role   = aws_iam_role.execution[each.key].id
  policy = data.aws_iam_policy_document.execution[each.key].json
}

resource "aws_iam_role" "task" {
  for_each = local.task_roles

  name               = "${local.role_names[each.key]}-task"
  assume_role_policy = data.aws_iam_policy_document.assume_task.json
}

data "aws_iam_policy_document" "task" {
  for_each = local.task_roles

  dynamic "statement" {
    for_each = each.value.state ? [1] : []
    content {
      actions   = ["elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"]
      resources = [aws_efs_file_system.state[0].arn]
      condition {
        test     = "StringEquals"
        variable = "elasticfilesystem:AccessPointArn"
        values = concat(
          [aws_efs_access_point.state[each.key].arn],
          [for k, ap in aws_efs_access_point.site : ap.arn if k == each.key],
        )
      }
    }
  }

  dynamic "statement" {
    for_each = each.value.s3_write == null ? [] : [each.value.s3_write]
    content {
      actions = concat(
        ["s3:PutObject"],
        statement.value.object_lock ? ["s3:PutObjectRetention"] : [],
      )
      resources = [
        "arn:${data.aws_partition.current.partition}:s3:::${statement.value.bucket}/${statement.value.prefix}/*"
      ]
    }
  }
}

resource "aws_iam_role_policy" "task" {
  for_each = local.task_roles

  name   = "task"
  role   = aws_iam_role.task[each.key].id
  policy = data.aws_iam_policy_document.task[each.key].json
}
