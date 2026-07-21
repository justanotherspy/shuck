# Variable surface, smallest first: for a personal deployment only the
# GitHub App credentials and the account/org choice are required — every
# secret shuck itself consumes (webhook, deliver, session) is generated
# in-stack unless supplied.

variable "region" {
  description = "AWS region. Null falls back to AWS_REGION / the active profile."
  type        = string
  default     = null
}

variable "name_prefix" {
  description = "Prefix for every resource name, so several stacks can share an account."
  type        = string
  default     = "shuck"
}

variable "tags" {
  description = "Extra tags applied to every resource."
  type        = map(string)
  default     = {}
}

# --- GitHub App (created manually first; see README) ---------------------

variable "github_app_id" {
  description = "The GitHub App's numeric ID."
  type        = number
}

variable "github_app_private_key" {
  description = "The GitHub App's private key PEM (generate on the App page)."
  type        = string
  sensitive   = true
}

variable "github_app_installation_id" {
  description = "The App installation's numeric ID (required in org mode; from the installation URL)."
  type        = number
  default     = 0
}

variable "github_client_id" {
  description = "The App's OAuth client ID (portal user-authorization flow)."
  type        = string
}

variable "github_client_secret" {
  description = "The App's OAuth client secret."
  type        = string
  sensitive   = true
}

variable "github_org" {
  description = "Org-install mode: the org whose members may mint tokens. Mutually exclusive with github_account_id."
  type        = string
  default     = ""
}

variable "github_account_id" {
  description = "Personal-install mode: the numeric GitHub user ID that may mint tokens. Mutually exclusive with github_org."
  type        = number
  default     = 0
}

variable "github_api_url" {
  description = "GitHub API base URL (GHES; empty means github.com)."
  type        = string
  default     = ""
}

variable "github_url" {
  description = "GitHub web origin (GHES; empty means https://github.com)."
  type        = string
  default     = ""
}

# --- Secrets (generated when empty) ---------------------------------------

variable "webhook_secret" {
  description = "GitHub webhook secret. Empty generates one — read it from the webhook_secret output and paste it into the App."
  type        = string
  sensitive   = true
  default     = ""
}

variable "deliver_secret_secondary" {
  description = "Second accepted deliver secret during a staged rotation (docs/RUNBOOK.md): set it to the old secret while workers move to the new one, then clear it. Empty means unset."
  type        = string
  sensitive   = true
  default     = ""
}

# --- Optional OIDC gate on the portal -------------------------------------

variable "oidc_issuer" {
  description = "OIDC issuer URL; enables the portal's SSO gate (all three oidc_* together or none)."
  type        = string
  default     = ""
}

variable "oidc_client_id" {
  description = "OIDC client ID."
  type        = string
  default     = ""
}

variable "oidc_client_secret" {
  description = "OIDC client secret."
  type        = string
  sensitive   = true
  default     = ""
}

# --- Worker tuning ---------------------------------------------------------

variable "ignore_authors" {
  description = "Comma-separated bot identities (numeric IDs and/or logins) whose review events are dropped. Set it to the App's bot identity, e.g. \"12345,my-shuck[bot]\", or agent-authored comments will loop."
  type        = string
  default     = ""
}

variable "review_context_lines" {
  description = "File lines around a review comment (0 keeps the binary's default, 10)."
  type        = number
  default     = 0
}

variable "summary_limit" {
  description = "Summary byte cap (0 keeps the binary's default, 16384)."
  type        = number
  default     = 0
}

# --- Retention & timing ------------------------------------------------------

variable "raw_log_retention_days" {
  description = "Days raw job logs stay in S3 before the lifecycle rule expires them. Retention lives here, never in worker code."
  type        = number
  default     = 1
}

variable "enable_raw_log_archive" {
  description = "Archive whole raw job logs to S3 (truncated summaries then carry an s3:// pointer)."
  type        = bool
  default     = true
}

variable "buffer_ttl" {
  description = "Buffered event retention (Go duration)."
  type        = string
  default     = "72h"
}

variable "grace_window" {
  description = "How long a disconnected subscriber keeps subscriptions and buffered events (Go duration)."
  type        = string
  default     = "24h"
}

variable "dedupe_ttl" {
  description = "Webhook delivery-GUID dedupe row retention (Go duration)."
  type        = string
  default     = "1h"
}

variable "gateway_sweep_schedule" {
  description = "EventBridge schedule for the gateway grace-window sweep."
  type        = string
  default     = "rate(15 minutes)"
}

variable "portal_sweep_schedule" {
  description = "EventBridge schedule for the portal token re-validation sweep."
  type        = string
  default     = "rate(1 day)"
}

variable "log_retention_days" {
  description = "CloudWatch log retention for every function."
  type        = number
  default     = 30
}

# --- Queue tuning ------------------------------------------------------------

variable "queue_visibility_timeout" {
  description = "SQS visibility timeout in seconds (>= 300 recommended: one jobs listing plus N log downloads per envelope). The worker Lambda timeout matches it."
  type        = number
  default     = 300

  validation {
    condition     = var.queue_visibility_timeout >= 60
    error_message = "queue_visibility_timeout must be at least 60 seconds."
  }
}

variable "queue_max_receive_count" {
  description = "Deliveries before an envelope lands in the DLQ (redrive policy)."
  type        = number
  default     = 5
}

# --- Observability (JUS-96, opt-in) ------------------------------------------

variable "observability" {
  description = <<-EOT
    Opt-in CloudWatch observability. Everything defaults off, so idle cost
    stays ~$0 and nothing changes unless you turn a knob on:

      - alarms_enabled:    DLQ-depth, per-Lambda error, and gateway-error alarms.
      - dashboard_enabled: a single CloudWatch dashboard for the whole stack.
      - xray_enabled:      Active X-Ray tracing on every Lambda (adds the
                           X-Ray write policy to each function role).
      - alarm_actions:     SNS topic (or other) ARNs notified on ALARM/OK.
                           Empty means the alarms still evaluate and show in the
                           console, they just take no action.
      - *_threshold:       alarm firing thresholds (per 5-minute period).
  EOT
  type = object({
    alarms_enabled           = optional(bool, false)
    dashboard_enabled        = optional(bool, false)
    xray_enabled             = optional(bool, false)
    alarm_actions            = optional(list(string), [])
    dlq_depth_threshold      = optional(number, 1)
    lambda_errors_threshold  = optional(number, 1)
    gateway_errors_threshold = optional(number, 1)
  })
  default = {}
}
