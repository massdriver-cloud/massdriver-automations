#!/usr/bin/env bash
# Usage: MASSDRIVER_SIGNING_SECRET=whsec_... scripts/send-event.sh event.json [url]
set -euo pipefail
body=$(cat "$1")
url=${2:-http://localhost:8080/hooks/massdriver}
id=$(jq -r .id <<<"$body")
ts=$(date +%s)
key=$(printf '%s' "${MASSDRIVER_SIGNING_SECRET#whsec_}" | base64 -d | xxd -p -c 256)
sig=$(printf '%s' "$id.$ts.$body" | openssl dgst -sha256 -mac HMAC -macopt "hexkey:$key" -binary | base64)
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$url" \
  -H 'content-type: application/cloudevents+json' \
  -H "webhook-id: $id" -H "webhook-timestamp: $ts" -H "webhook-signature: v1,$sig" \
  --data-binary "$body"
