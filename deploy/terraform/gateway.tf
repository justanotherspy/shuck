# The serverless gateway: an API Gateway WebSocket API terminates the shim
# connections, one Lambda (role "ws") serves its $connect/$default/
# $disconnect routes, a second (role "deliver") serves the workers'
# POST /internal/deliver behind a function URL, and a third (role "sweep")
# runs the grace-window sweep on an EventBridge schedule. All three run the
# same shuck-gateway binary, dispatched by SHUCK_WS_ROLE.

resource "aws_apigatewayv2_api" "ws" {
  name                       = "${var.name_prefix}-gateway"
  protocol_type              = "WEBSOCKET"
  route_selection_expression = "$request.body.type"
}

resource "aws_apigatewayv2_stage" "ws" {
  api_id      = aws_apigatewayv2_api.ws.id
  name        = "ws"
  auto_deploy = true
}

locals {
  # The @connections callback endpoint the gateway pushes through, and the
  # wss:// URL shims dial.
  ws_endpoint = "https://${aws_apigatewayv2_api.ws.id}.execute-api.${data.aws_region.current.region}.amazonaws.com/${aws_apigatewayv2_stage.ws.name}"
  ws_url      = "wss://${aws_apigatewayv2_api.ws.id}.execute-api.${data.aws_region.current.region}.amazonaws.com/${aws_apigatewayv2_stage.ws.name}"

  gateway_table_env = {
    SHUCK_TOKEN_TABLE        = aws_dynamodb_table.tokens.name
    SHUCK_SUBSCRIPTION_TABLE = aws_dynamodb_table.subscriptions.name
    SHUCK_BUFFER_TABLE       = aws_dynamodb_table.buffer.name
    SHUCK_BUFFER_TTL         = var.buffer_ttl
  }
}

resource "aws_cloudwatch_log_group" "gateway_ws" {
  name              = "/aws/lambda/${var.name_prefix}-gateway-ws"
  retention_in_days = var.log_retention_days
}

resource "aws_lambda_function" "gateway_ws" {
  function_name = "${var.name_prefix}-gateway-ws"
  role          = aws_iam_role.lambda["gateway_ws"].arn

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
  timeout       = 30

  filename         = data.archive_file.lambda["gateway"].output_path
  source_code_hash = data.archive_file.lambda["gateway"].output_base64sha256

  environment {
    variables = merge(local.gateway_table_env, {
      SHUCK_WS_ROLE     = "ws"
      SHUCK_WS_ENDPOINT = local.ws_endpoint
    })
  }

  depends_on = [aws_cloudwatch_log_group.gateway_ws]
}

resource "aws_lambda_permission" "gateway_ws" {
  statement_id  = "AllowAPIGatewayWebSocket"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.gateway_ws.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.ws.execution_arn}/*"
}

resource "aws_apigatewayv2_integration" "ws" {
  api_id           = aws_apigatewayv2_api.ws.id
  integration_type = "AWS_PROXY"
  integration_uri  = aws_lambda_function.gateway_ws.invoke_arn
}

resource "aws_apigatewayv2_route" "ws" {
  for_each = toset(["$connect", "$default", "$disconnect"])

  api_id    = aws_apigatewayv2_api.ws.id
  route_key = each.key
  target    = "integrations/${aws_apigatewayv2_integration.ws.id}"
}

# --- deliver -----------------------------------------------------------------

resource "aws_cloudwatch_log_group" "gateway_deliver" {
  name              = "/aws/lambda/${var.name_prefix}-gateway-deliver"
  retention_in_days = var.log_retention_days
}

resource "aws_lambda_function" "gateway_deliver" {
  function_name = "${var.name_prefix}-gateway-deliver"
  role          = aws_iam_role.lambda["gateway_deliver"].arn

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
  timeout       = 30

  filename         = data.archive_file.lambda["gateway"].output_path
  source_code_hash = data.archive_file.lambda["gateway"].output_base64sha256

  environment {
    variables = merge(
      local.gateway_table_env,
      {
        SHUCK_WS_ROLE        = "deliver"
        SHUCK_WS_ENDPOINT    = local.ws_endpoint
        SHUCK_DELIVER_SECRET = local.deliver_secret
      },
      # Staged rotation: the deliver role (the only secret validator) also
      # accepts the secondary while workers move to the new secret.
      var.deliver_secret_secondary != "" ? {
        SHUCK_DELIVER_SECRET_SECONDARY = var.deliver_secret_secondary
      } : {},
    )
  }

  depends_on = [aws_cloudwatch_log_group.gateway_deliver]
}

# Auth is the constant-time X-Shuck-Deliver-Secret check inside the handler
# — the same discipline as the resident gateway, whose /internal/deliver is
# also network-reachable in chart deployments.
resource "aws_lambda_function_url" "gateway_deliver" {
  function_name      = aws_lambda_function.gateway_deliver.function_name
  authorization_type = "NONE"
}

# --- sweep ---------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "gateway_sweep" {
  name              = "/aws/lambda/${var.name_prefix}-gateway-sweep"
  retention_in_days = var.log_retention_days
}

resource "aws_lambda_function" "gateway_sweep" {
  function_name = "${var.name_prefix}-gateway-sweep"
  role          = aws_iam_role.lambda["gateway_sweep"].arn

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
  timeout       = 300

  filename         = data.archive_file.lambda["gateway"].output_path
  source_code_hash = data.archive_file.lambda["gateway"].output_base64sha256

  environment {
    variables = merge(local.gateway_table_env, {
      SHUCK_WS_ROLE      = "sweep"
      SHUCK_GRACE_WINDOW = var.grace_window
    })
  }

  depends_on = [aws_cloudwatch_log_group.gateway_sweep]
}

resource "aws_cloudwatch_event_rule" "gateway_sweep" {
  name                = "${var.name_prefix}-gateway-sweep"
  schedule_expression = var.gateway_sweep_schedule
}

resource "aws_cloudwatch_event_target" "gateway_sweep" {
  rule = aws_cloudwatch_event_rule.gateway_sweep.name
  arn  = aws_lambda_function.gateway_sweep.arn
}

resource "aws_lambda_permission" "gateway_sweep" {
  statement_id  = "AllowEventBridge"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.gateway_sweep.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.gateway_sweep.arn
}
