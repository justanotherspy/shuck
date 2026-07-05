# The event worker: SQS event source mapping → Lambda. ReportBatchItemFailures
# is load-bearing (JUS-87 hard requirement): the handler always returns a
# partial-batch response, and without the flag Lambda would delete failed
# records along with the rest of the batch.

resource "aws_cloudwatch_log_group" "worker" {
  name              = "/aws/lambda/${var.name_prefix}-worker"
  retention_in_days = var.log_retention_days
}

resource "aws_lambda_function" "worker" {
  function_name = "${var.name_prefix}-worker"
  role          = aws_iam_role.lambda["worker"].arn
  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["arm64"]
  memory_size   = 256
  timeout       = var.queue_visibility_timeout

  filename         = data.archive_file.lambda["worker"].output_path
  source_code_hash = data.archive_file.lambda["worker"].output_base64sha256

  environment {
    variables = merge(
      {
        SHUCK_GITHUB_APP_ID          = tostring(var.github_app_id)
        SHUCK_GITHUB_APP_PRIVATE_KEY = var.github_app_private_key
        SHUCK_DELIVER_URL            = "${aws_lambda_function_url.gateway_deliver.function_url}internal/deliver"
        SHUCK_DELIVER_SECRET         = local.deliver_secret
      },
      var.enable_raw_log_archive ? { SHUCK_RAW_LOG_BUCKET = aws_s3_bucket.raw_logs[0].bucket } : {},
      var.ignore_authors != "" ? { SHUCK_IGNORE_AUTHORS = var.ignore_authors } : {},
      var.review_context_lines > 0 ? { SHUCK_REVIEW_CONTEXT_LINES = tostring(var.review_context_lines) } : {},
      var.summary_limit > 0 ? { SHUCK_SUMMARY_LIMIT = tostring(var.summary_limit) } : {},
      var.github_api_url != "" ? { SHUCK_GITHUB_API_URL = var.github_api_url } : {},
    )
  }

  depends_on = [aws_cloudwatch_log_group.worker]
}

resource "aws_lambda_event_source_mapping" "worker" {
  event_source_arn        = aws_sqs_queue.events.arn
  function_name           = aws_lambda_function.worker.arn
  batch_size              = 5
  function_response_types = ["ReportBatchItemFailures"]
}
