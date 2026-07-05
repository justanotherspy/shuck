# Lambda artifacts are built from this working tree at apply time: the repo
# is the distribution unit, so a fresh clone plus `terraform apply` deploys
# exactly the code you can read. Prerequisite: a Go toolchain matching
# go.mod (the build script uses GOTOOLCHAIN=auto, so any recent Go
# self-upgrades). Zips rebuild only when Go sources change.

locals {
  components = toset(["ingest", "worker", "gateway", "portal"])

  # A content hash over everything that reaches the binaries; any change
  # replaces the build step and re-zips.
  source_hash = sha1(join("", concat(
    [for f in sort(fileset("${path.module}/../..", "{cmd,internal}/**/*.go")) : filesha1("${path.module}/../../${f}")],
    [
      filesha1("${path.module}/../../go.mod"),
      filesha1("${path.module}/../../go.sum"),
      filesha1("${path.module}/scripts/build.sh"),
    ],
  )))
}

resource "terraform_data" "build" {
  for_each = local.components

  triggers_replace = local.source_hash

  provisioner "local-exec" {
    command = "${abspath(path.module)}/scripts/build.sh ${each.key} ${abspath(path.module)}/build"
  }
}

data "archive_file" "lambda" {
  for_each = local.components

  type        = "zip"
  source_file = "${path.module}/build/${each.key}/bootstrap"
  output_path = "${path.module}/build/${each.key}.zip"

  depends_on = [terraform_data.build]
}
