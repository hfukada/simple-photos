#!/bin/sh
# Prepares MinIO for simple-photos. Run by the minio-init compose service.
set -eu

mc alias set local http://minio:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null

mc mb --ignore-existing "local/$S3_BUCKET"
mc anonymous set none "local/$S3_BUCKET"

# Backstop for the app's own reaper: delete objects 31 days after upload.
# `ilm import` replaces the bucket's rules, so re-running is idempotent.
cat > /tmp/lifecycle.json <<JSON
{"Rules": [{"ID": "simple-photos-expire", "Status": "Enabled",
            "Filter": {"Prefix": ""}, "Expiration": {"Days": 31}}]}
JSON
mc ilm import "local/$S3_BUCKET" < /tmp/lifecycle.json

# Dedicated access key that can only touch objects in this bucket.
cat > /tmp/policy.json <<JSON
{"Version": "2012-10-17", "Statement": [{
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
  "Resource": ["arn:aws:s3:::$S3_BUCKET/*"]}]}
JSON
mc admin user add local "$S3_ACCESS_KEY" "$S3_SECRET_KEY"
mc admin policy create local simple-photos-rw /tmp/policy.json
mc admin policy attach local simple-photos-rw --user "$S3_ACCESS_KEY" 2>/dev/null || true

echo "minio ready: bucket=$S3_BUCKET user=$S3_ACCESS_KEY"
