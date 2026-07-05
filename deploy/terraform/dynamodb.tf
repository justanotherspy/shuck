# The four tables, shaped exactly as the adapters expect (the frozen
# schemas from docs/V2.md § JUS-88 plus the ingest dedupe table). All
# on-demand: idle cost is zero and no capacity math is required.

# tokens: pk = hex sha256 of the bearer token. Written by the portal, read
# by the gateway (revocation = row delete).
resource "aws_dynamodb_table" "tokens" {
  name         = "${var.name_prefix}-tokens"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"

  attribute {
    name = "pk"
    type = "S"
  }
}

# subscriptions: pk = "owner/name#pr", sk = "user_id#session_id", plus the
# KEYS_ONLY reverse index the sweep and ingest pre-filter use.
resource "aws_dynamodb_table" "subscriptions" {
  name         = "${var.name_prefix}-subscriptions"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }

  attribute {
    name = "sk"
    type = "S"
  }

  global_secondary_index {
    name            = "subscriber-index"
    hash_key        = "sk"
    range_key       = "pk"
    projection_type = "KEYS_ONLY"
  }
}

# buffer: pk = "user_id#session_id"; the sort key discriminates event rows
# (s#<seq>), the seq counter (c), event-id dedupe markers (e#<id>), presence
# (p), and the serverless connection registry (w, plus conn#<id> reverse
# partitions). TTL on expires prunes everything.
resource "aws_dynamodb_table" "buffer" {
  name         = "${var.name_prefix}-buffer"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }

  attribute {
    name = "sk"
    type = "S"
  }

  ttl {
    attribute_name = "expires"
    enabled        = true
  }
}

# dedupe: pk = webhook delivery GUID; conditional put drops redeliveries.
resource "aws_dynamodb_table" "dedupe" {
  name         = "${var.name_prefix}-dedupe"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"

  attribute {
    name = "pk"
    type = "S"
  }

  ttl {
    attribute_name = "expires"
    enabled        = true
  }
}
