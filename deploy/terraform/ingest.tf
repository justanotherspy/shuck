# The webhook ingest: a Lambda behind a public function URL. Authorization
# is the HMAC signature check over the raw body (X-Hub-Signature-256) inside
# the handler — unsigned or mismatched deliveries are rejected before the
# payload is parsed.

resource "aws_cloudwatch_log_group" "ingest" {
  name              = "/aws/lambda/${var.name_prefix}-ingest"
  retention_in_days = var.log_retention_days
}

resource "aws_lambda_function" "ingest" {
  function_name = "${var.name_prefix}-ingest"
  role          = aws_iam_role.lambda["ingest"].arn

  dynamic "tracing_config" {
    for_each = local.xray_tracing
    content {
      mode = tracing_config.value
    }
  }

  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["arm64"]
  memory_size   = 128
  timeout       = 10

  filename         = data.archive_file.lambda["ingest"].output_path
  source_code_hash = data.archive_file.lambda["ingest"].output_base64sha256

  environment {
    variables = {
      SHUCK_WEBHOOK_SECRET     = local.webhook_secret
      SHUCK_QUEUE_URL          = aws_sqs_queue.events.url
      SHUCK_DEDUPE_TABLE       = aws_dynamodb_table.dedupe.name
      SHUCK_DEDUPE_TTL         = var.dedupe_ttl
      SHUCK_SUBSCRIPTION_TABLE = aws_dynamodb_table.subscriptions.name
    }
  }

  # Deploy order (JUS-91 hard requirement): the worker must update before
  # ingest, or an old worker treats new envelope kinds as poison and review
  # events redeliver into the DLQ until it catches up.
  depends_on = [
    aws_cloudwatch_log_group.ingest,
    aws_lambda_function.worker,
    aws_lambda_event_source_mapping.worker,
  ]
}

resource "aws_lambda_function_url" "ingest" {
  function_name      = aws_lambda_function.ingest.function_name
  authorization_type = "NONE"
}
