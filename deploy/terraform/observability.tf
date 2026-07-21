# Opt-in CloudWatch observability (JUS-96): alarms, a dashboard, and X-Ray
# tracing. All off by default — the base stack ships a log group per Lambda
# and nothing else, so idle cost stays ~$0. The counters the resident
# binaries expose over /metrics (the Helm target) have no home in the
# serverless target; CloudWatch metrics are the serverless equivalent.

locals {
  # X-Ray tracing mode list, consumed by each Lambda's dynamic
  # tracing_config block (empty disables the block entirely).
  xray_tracing = var.observability.xray_enabled ? ["Active"] : []

  # Every function, for per-Lambda error alarms and the dashboard.
  lambda_functions = {
    ingest          = aws_lambda_function.ingest.function_name
    worker          = aws_lambda_function.worker.function_name
    gateway_ws      = aws_lambda_function.gateway_ws.function_name
    gateway_deliver = aws_lambda_function.gateway_deliver.function_name
    gateway_sweep   = aws_lambda_function.gateway_sweep.function_name
    portal          = aws_lambda_function.portal.function_name
    portal_sweep    = aws_lambda_function.portal_sweep.function_name
  }
}

# X-Ray write permission for every Lambda role when tracing is enabled. The
# managed policy is the AWS-recommended grant for the X-Ray SDK's segment
# uploads.
resource "aws_iam_role_policy_attachment" "lambda_xray" {
  for_each = var.observability.xray_enabled ? aws_iam_role.lambda : {}

  role       = each.value.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

# The DLQ is the single highest-signal alarm: a non-empty DLQ means poison
# envelopes are piling up (usually an old worker meeting a new envelope kind —
# see the deploy-order contract). This is the alarm the README used to tell
# operators to build by hand.
resource "aws_cloudwatch_metric_alarm" "dlq_depth" {
  count = var.observability.alarms_enabled ? 1 : 0

  alarm_name          = "${var.name_prefix}-dlq-depth"
  alarm_description   = "Events dead-letter queue is non-empty: poison envelopes are accumulating (often an old worker + a new envelope kind). Redrive after fixing the worker."
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.events_dlq.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = var.observability.dlq_depth_threshold
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.observability.alarm_actions
  ok_actions          = var.observability.alarm_actions
}

# One error alarm per Lambda. Sum of the AWS/Lambda Errors metric over the
# period; a handler that throws (or a poison envelope the worker rejects)
# shows here before it reaches the DLQ.
resource "aws_cloudwatch_metric_alarm" "lambda_errors" {
  for_each = var.observability.alarms_enabled ? local.lambda_functions : {}

  alarm_name          = "${var.name_prefix}-${replace(each.key, "_", "-")}-errors"
  alarm_description   = "Lambda ${each.value} is returning errors."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = each.value }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = var.observability.lambda_errors_threshold
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.observability.alarm_actions
  ok_actions          = var.observability.alarm_actions
}

# Server-side errors on the gateway WebSocket API (the serverless analogue of
# "gateway 5xx"): ExecutionError counts integration/handler failures API
# Gateway saw while servicing shim frames.
resource "aws_cloudwatch_metric_alarm" "gateway_ws_errors" {
  count = var.observability.alarms_enabled ? 1 : 0

  alarm_name          = "${var.name_prefix}-gateway-ws-errors"
  alarm_description   = "The gateway WebSocket API is reporting execution errors servicing shim connections."
  namespace           = "AWS/ApiGateway"
  metric_name         = "ExecutionError"
  dimensions          = { ApiId = aws_apigatewayv2_api.ws.id, Stage = aws_apigatewayv2_stage.ws.name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = var.observability.gateway_errors_threshold
  treat_missing_data  = "notBreaching"
  alarm_actions       = var.observability.alarm_actions
  ok_actions          = var.observability.alarm_actions
}

# One dashboard for the whole stack: queue depth, per-Lambda throughput and
# errors, and WebSocket API traffic.
resource "aws_cloudwatch_dashboard" "main" {
  count = var.observability.dashboard_enabled ? 1 : 0

  dashboard_name = var.name_prefix
  dashboard_body = jsonencode({
    widgets = [
      {
        type   = "metric"
        x      = 0
        y      = 0
        width  = 12
        height = 6
        properties = {
          title  = "Event queue depth"
          region = var.region
          view   = "timeSeries"
          stat   = "Maximum"
          period = 300
          metrics = [
            ["AWS/SQS", "ApproximateNumberOfMessagesVisible", "QueueName", aws_sqs_queue.events.name, { label = "events" }],
            ["AWS/SQS", "ApproximateNumberOfMessagesVisible", "QueueName", aws_sqs_queue.events_dlq.name, { label = "dlq" }],
          ]
        }
      },
      {
        type   = "metric"
        x      = 12
        y      = 0
        width  = 12
        height = 6
        properties = {
          title  = "Lambda errors"
          region = var.region
          view   = "timeSeries"
          stat   = "Sum"
          period = 300
          metrics = [
            for key, name in local.lambda_functions :
            ["AWS/Lambda", "Errors", "FunctionName", name, { label = key }]
          ]
        }
      },
      {
        type   = "metric"
        x      = 0
        y      = 6
        width  = 12
        height = 6
        properties = {
          title  = "Lambda invocations"
          region = var.region
          view   = "timeSeries"
          stat   = "Sum"
          period = 300
          metrics = [
            for key, name in local.lambda_functions :
            ["AWS/Lambda", "Invocations", "FunctionName", name, { label = key }]
          ]
        }
      },
      {
        type   = "metric"
        x      = 12
        y      = 6
        width  = 12
        height = 6
        properties = {
          title  = "Gateway WebSocket API"
          region = var.region
          view   = "timeSeries"
          stat   = "Sum"
          period = 300
          metrics = [
            ["AWS/ApiGateway", "ConnectCount", "ApiId", aws_apigatewayv2_api.ws.id, "Stage", aws_apigatewayv2_stage.ws.name, { label = "connects" }],
            ["AWS/ApiGateway", "MessageCount", "ApiId", aws_apigatewayv2_api.ws.id, "Stage", aws_apigatewayv2_stage.ws.name, { label = "messages" }],
            ["AWS/ApiGateway", "ExecutionError", "ApiId", aws_apigatewayv2_api.ws.id, "Stage", aws_apigatewayv2_stage.ws.name, { label = "errors" }],
          ]
        }
      },
    ]
  })
}
