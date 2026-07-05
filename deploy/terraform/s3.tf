# Raw job-log archive. Retention is this lifecycle rule and nothing else —
# the worker only writes (JUS-87 contract: retention never lives in worker
# code). Truncated summaries carry the s3:// pointer so a reading agent can
# fetch the whole log while it lives.

resource "aws_s3_bucket" "raw_logs" {
  count = var.enable_raw_log_archive ? 1 : 0

  bucket_prefix = "${var.name_prefix}-raw-logs-"
  force_destroy = true # logs are 24h-lived caches; terraform destroy must leave nothing
}

resource "aws_s3_bucket_public_access_block" "raw_logs" {
  count = var.enable_raw_log_archive ? 1 : 0

  bucket                  = aws_s3_bucket.raw_logs[0].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "raw_logs" {
  count = var.enable_raw_log_archive ? 1 : 0

  bucket = aws_s3_bucket.raw_logs[0].id

  rule {
    id     = "expire-raw-logs"
    status = "Enabled"

    filter {
      prefix = "raw/"
    }

    expiration {
      days = var.raw_log_retention_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}
