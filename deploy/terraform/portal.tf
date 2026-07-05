# The token portal: a Lambda behind an API Gateway HTTP API (TLS, which the
# portal's Secure session cookies require), plus the daily re-validation
# sweep on an EventBridge schedule. Both run the same shuck-portal binary;
# SHUCK_PORTAL_ROLE picks the sweep.
#
# An HTTP API rather than a function URL on purpose: the portal must know
# its own external origin (SHUCK_BASE_URL feeds the OAuth callback URLs),
# and a function URL's subdomain is minted from its function — a dependency
# cycle. The HTTP API's endpoint exists before the function does, so the
# origin is a plain reference. HTTP API v2 events are the same payload
# format function URLs use, so the binary's Lambda adapter serves both.

resource "aws_apigatewayv2_api" "portal" {
  name          = "${var.name_prefix}-portal"
  protocol_type = "HTTP"
}

resource "aws_apigatewayv2_stage" "portal" {
  api_id      = aws_apigatewayv2_api.portal.id
  name        = "$default"
  auto_deploy = true
}

locals {
  portal_url = aws_apigatewayv2_api.portal.api_endpoint
}

resource "aws_cloudwatch_log_group" "portal" {
  name              = "/aws/lambda/${var.name_prefix}-portal"
  retention_in_days = var.log_retention_days
}

resource "aws_lambda_function" "portal" {
  function_name = "${var.name_prefix}-portal"
  role          = aws_iam_role.lambda["portal"].arn
  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["arm64"]
  memory_size   = 128
  timeout       = 30

  filename         = data.archive_file.lambda["portal"].output_path
  source_code_hash = data.archive_file.lambda["portal"].output_base64sha256

  environment {
    variables = merge(
      local.portal_common_env,
      {
        SHUCK_BASE_URL             = local.portal_url
        SHUCK_SESSION_SECRET       = local.session_secret
        SHUCK_GITHUB_CLIENT_ID     = var.github_client_id
        SHUCK_GITHUB_CLIENT_SECRET = var.github_client_secret
      },
      var.github_url != "" ? { SHUCK_GITHUB_URL = var.github_url } : {},
      var.oidc_issuer != "" ? {
        SHUCK_OIDC_ISSUER        = var.oidc_issuer
        SHUCK_OIDC_CLIENT_ID     = var.oidc_client_id
        SHUCK_OIDC_CLIENT_SECRET = var.oidc_client_secret
      } : {},
    )
  }

  lifecycle {
    precondition {
      condition     = (var.github_org != "") != (var.github_account_id != 0)
      error_message = "Set exactly one of github_org (org-install mode) or github_account_id (personal-install mode)."
    }
    precondition {
      condition     = var.github_org == "" || var.github_app_installation_id != 0
      error_message = "github_app_installation_id is required in org mode (github_org set)."
    }
    precondition {
      condition     = (var.oidc_issuer != "") == (var.oidc_client_id != "") && (var.oidc_issuer != "") == (var.oidc_client_secret != "")
      error_message = "Set oidc_issuer, oidc_client_id, and oidc_client_secret together, or none of them."
    }
  }

  depends_on = [aws_cloudwatch_log_group.portal]
}

resource "aws_apigatewayv2_integration" "portal" {
  api_id                 = aws_apigatewayv2_api.portal.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.portal.invoke_arn
  payload_format_version = "2.0"
}

resource "aws_apigatewayv2_route" "portal" {
  api_id    = aws_apigatewayv2_api.portal.id
  route_key = "$default"
  target    = "integrations/${aws_apigatewayv2_integration.portal.id}"
}

resource "aws_lambda_permission" "portal" {
  statement_id  = "AllowAPIGatewayHTTP"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.portal.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.portal.execution_arn}/*"
}

# --- sweep ---------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "portal_sweep" {
  name              = "/aws/lambda/${var.name_prefix}-portal-sweep"
  retention_in_days = var.log_retention_days
}

resource "aws_lambda_function" "portal_sweep" {
  function_name = "${var.name_prefix}-portal-sweep"
  role          = aws_iam_role.lambda["portal_sweep"].arn
  runtime       = "provided.al2023"
  handler       = "bootstrap"
  architectures = ["arm64"]
  memory_size   = 128
  timeout       = 300

  filename         = data.archive_file.lambda["portal"].output_path
  source_code_hash = data.archive_file.lambda["portal"].output_base64sha256

  environment {
    variables = merge(local.portal_common_env, {
      SHUCK_PORTAL_ROLE = "sweep"
    })
  }

  depends_on = [aws_cloudwatch_log_group.portal_sweep]
}

resource "aws_cloudwatch_event_rule" "portal_sweep" {
  name                = "${var.name_prefix}-portal-sweep"
  schedule_expression = var.portal_sweep_schedule
}

resource "aws_cloudwatch_event_target" "portal_sweep" {
  rule = aws_cloudwatch_event_rule.portal_sweep.name
  arn  = aws_lambda_function.portal_sweep.arn
}

resource "aws_lambda_permission" "portal_sweep" {
  statement_id  = "AllowEventBridge"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.portal_sweep.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.portal_sweep.arn
}
