data "aws_region" "current" {}

data "aws_partition" "current" {}

locals {
  # Validation mode: exactly one of org / personal account (checked by a
  # precondition on the portal function).
  org_mode = var.github_org != ""

  # Generated-unless-supplied secrets.
  webhook_secret = var.webhook_secret != "" ? var.webhook_secret : random_password.webhook_secret[0].result
  deliver_secret = random_password.deliver_secret.result
  session_secret = random_password.session_secret.result

  # Env vars shared by both portal roles (serve validates + mints; sweep
  # only re-validates, but the extra vars are harmless and keep one map).
  portal_common_env = merge(
    {
      SHUCK_TOKEN_TABLE = aws_dynamodb_table.tokens.name
    },
    local.org_mode ? {
      SHUCK_GITHUB_ORG             = var.github_org
      SHUCK_GITHUB_APP_ID          = tostring(var.github_app_id)
      SHUCK_GITHUB_APP_PRIVATE_KEY = var.github_app_private_key
      SHUCK_GITHUB_INSTALLATION_ID = tostring(var.github_app_installation_id)
      } : {
      SHUCK_GITHUB_ACCOUNT_ID = tostring(var.github_account_id)
    },
    var.github_api_url != "" ? { SHUCK_GITHUB_API_URL = var.github_api_url } : {},
  )
}

resource "random_password" "webhook_secret" {
  count   = var.webhook_secret == "" ? 1 : 0
  length  = 40
  special = false
}

resource "random_password" "deliver_secret" {
  length  = 48
  special = false
}

resource "random_password" "session_secret" {
  length  = 48
  special = false
}
