#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
API_CHECK="false"

for arg in "$@"; do
  case "$arg" in
    --api-check) API_CHECK="true" ;;
    *)
      echo "Unknown arg: $arg" >&2
      exit 2
      ;;
  esac
done

cd "$ROOT_DIR"

echo "[1/3] go test ./..."
go test ./...

echo "[2/3] bun run admin:typecheck"
bun run admin:typecheck

echo "[3/3] bun run admin:build"
bun run admin:build

if [[ "$API_CHECK" == "true" ]]; then
  : "${ADMIN_URL:?Set ADMIN_URL for --api-check}"
  : "${ADMIN_BACKEND_BOT_TOKEN:?Set ADMIN_BACKEND_BOT_TOKEN for --api-check}"

  payload='{"chatId":"health-check","message":"ping","lang":"en"}'

  echo "[api-check] POST /api/codex/chat without token (expect 401)"
  status_no_token="$(curl -sS -o /tmp/lx_codex_no_token.json -w '%{http_code}' -X POST "$ADMIN_URL/api/codex/chat" -H 'Content-Type: application/json' --data "$payload")"
  if [[ "$status_no_token" != "401" ]]; then
    echo "unexpected status without token: $status_no_token" >&2
    cat /tmp/lx_codex_no_token.json >&2 || true
    exit 1
  fi

  echo "[api-check] POST /api/codex/chat with token (expect 200)"
  status_with_token="$(curl -sS -o /tmp/lx_codex_with_token.json -w '%{http_code}' -X POST "$ADMIN_URL/api/codex/chat" -H 'Content-Type: application/json' -H "X-Admin-Bot-Token: $ADMIN_BACKEND_BOT_TOKEN" --data "$payload")"
  if [[ "$status_with_token" != "200" ]]; then
    echo "unexpected status with token: $status_with_token" >&2
    cat /tmp/lx_codex_with_token.json >&2 || true
    exit 1
  fi

  if ! rg -q '"ok"\s*:\s*true' /tmp/lx_codex_with_token.json; then
    echo "api response missing ok:true" >&2
    cat /tmp/lx_codex_with_token.json >&2 || true
    exit 1
  fi
fi

echo "READY: validation completed"
