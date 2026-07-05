output "webhook_url" {
  description = "Paste into the GitHub App's Webhook URL field."
  value       = "${aws_lambda_function_url.ingest.function_url}webhook"
}

output "webhook_secret" {
  description = "Paste into the GitHub App's Webhook secret field (generated unless you supplied one). Read with: terraform output -raw webhook_secret"
  value       = local.webhook_secret
  sensitive   = true
}

output "portal_url" {
  description = "The token portal. Register \"<portal_url>/github/callback\" as the App's OAuth callback URL, then mint tokens here."
  value       = local.portal_url
}

output "gateway_ws_url" {
  description = "The gateway WebSocket URL for the channel shim (SHUCK_CHANNEL_GATEWAY_URL)."
  value       = local.ws_url
}

output "queue_name" {
  description = "The envelope queue."
  value       = aws_sqs_queue.events.name
}

output "dlq_name" {
  description = "Poison envelopes land here (redrive policy); alarm on its depth."
  value       = aws_sqs_queue.events_dlq.name
}

output "raw_log_bucket" {
  description = "Raw job-log archive bucket (empty when archiving is disabled)."
  value       = var.enable_raw_log_archive ? aws_s3_bucket.raw_logs[0].bucket : ""
}
