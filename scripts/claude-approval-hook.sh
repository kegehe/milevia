#!/bin/sh
set -eu

: "${AUTO_CONTROL_URL:?AUTO_CONTROL_URL is required}"
: "${AUTO_APPROVAL_TOKEN:?AUTO_APPROVAL_TOKEN is required}"

if [ -n "${AUTO_APPROVAL_CONVERSATION_ID:-}" ]; then
  approval_identity_header="X-Auto-Conversation-ID: ${AUTO_APPROVAL_CONVERSATION_ID}"
elif [ -n "${AUTO_APPROVAL_RUN_ID:-}" ]; then
  approval_identity_header="X-Auto-Run-ID: ${AUTO_APPROVAL_RUN_ID}"
else
  echo "AUTO_APPROVAL_CONVERSATION_ID or AUTO_APPROVAL_RUN_ID is required" >&2
  exit 1
fi

exec curl --fail --silent --show-error --max-time 305 \
  -X POST "${AUTO_CONTROL_URL}/api/internal/approvals/wait" \
  -H "Content-Type: application/json" \
  -H "${approval_identity_header}" \
  -H "X-Auto-Approval-Token: ${AUTO_APPROVAL_TOKEN}" \
  --data-binary @-
