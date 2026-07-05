# One role per function, least-privilege by resource: each component can
# reach exactly the tables/queue/bucket/API it owns and nothing else. The
# gateway roles share one DynamoDB statement over the gateway tables (their
# access patterns interleave on the same rows); everything else is scoped
# per action.

data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

locals {
  gateway_table_arns = [
    aws_dynamodb_table.tokens.arn,
    aws_dynamodb_table.subscriptions.arn,
    "${aws_dynamodb_table.subscriptions.arn}/index/*",
    aws_dynamodb_table.buffer.arn,
  ]

  gateway_ddb_actions = [
    "dynamodb:GetItem",
    "dynamodb:PutItem",
    "dynamodb:UpdateItem",
    "dynamodb:DeleteItem",
    "dynamodb:Query",
    "dynamodb:Scan",
    "dynamodb:BatchWriteItem",
    "dynamodb:TransactWriteItems",
  ]

  # policy statements per role; roles are created for_each over this map.
  role_statements = {
    ingest = [
      {
        actions   = ["dynamodb:PutItem", "dynamodb:DeleteItem"]
        resources = [aws_dynamodb_table.dedupe.arn]
      },
      {
        actions   = ["dynamodb:Query"]
        resources = [aws_dynamodb_table.subscriptions.arn]
      },
      {
        actions   = ["sqs:SendMessage"]
        resources = [aws_sqs_queue.events.arn]
      },
    ]

    worker = concat(
      [
        {
          actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
          resources = [aws_sqs_queue.events.arn]
        },
      ],
      var.enable_raw_log_archive ? [
        {
          actions   = ["s3:PutObject"]
          resources = ["${aws_s3_bucket.raw_logs[0].arn}/raw/*"]
        },
      ] : [],
    )

    gateway_ws = [
      {
        actions   = local.gateway_ddb_actions
        resources = local.gateway_table_arns
      },
      {
        actions   = ["execute-api:ManageConnections"]
        resources = ["${aws_apigatewayv2_api.ws.execution_arn}/*"]
      },
    ]

    gateway_deliver = [
      {
        actions   = local.gateway_ddb_actions
        resources = local.gateway_table_arns
      },
      {
        actions   = ["execute-api:ManageConnections"]
        resources = ["${aws_apigatewayv2_api.ws.execution_arn}/*"]
      },
    ]

    gateway_sweep = [
      {
        actions   = local.gateway_ddb_actions
        resources = local.gateway_table_arns
      },
    ]

    portal = [
      {
        actions = [
          "dynamodb:GetItem",
          "dynamodb:PutItem",
          "dynamodb:DeleteItem",
          "dynamodb:Scan",
          "dynamodb:TransactWriteItems",
        ]
        resources = [aws_dynamodb_table.tokens.arn]
      },
    ]

    portal_sweep = [
      {
        actions = [
          "dynamodb:GetItem",
          "dynamodb:DeleteItem",
          "dynamodb:Scan",
          "dynamodb:TransactWriteItems",
        ]
        resources = [aws_dynamodb_table.tokens.arn]
      },
    ]
  }
}

resource "aws_iam_role" "lambda" {
  for_each = local.role_statements

  name               = "${var.name_prefix}-${replace(each.key, "_", "-")}"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
}

resource "aws_iam_role_policy_attachment" "lambda_logs" {
  for_each = local.role_statements

  role       = aws_iam_role.lambda[each.key].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

data "aws_iam_policy_document" "lambda" {
  for_each = local.role_statements

  dynamic "statement" {
    for_each = each.value

    content {
      actions   = statement.value.actions
      resources = statement.value.resources
    }
  }
}

resource "aws_iam_role_policy" "lambda" {
  for_each = local.role_statements

  name   = "${var.name_prefix}-${replace(each.key, "_", "-")}"
  role   = aws_iam_role.lambda[each.key].id
  policy = data.aws_iam_policy_document.lambda[each.key].json
}
