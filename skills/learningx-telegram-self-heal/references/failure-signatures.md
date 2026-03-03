# Failure Signatures

## 1) Telegram polling conflict (`getUpdates` 409)

Symptoms:
- `getUpdates error 409: Conflict: terminated by other getUpdates request`

Likely causes:
- Multiple running bot instances using long-polling simultaneously.
- A webhook is set while long-polling is also active.

Fix direction:
- Ensure single polling instance.
- If polling mode is used, clear webhook.
- Confirm only one Railway replica for bot polling path.

## 2) Bot reply shows `Unauthorized.`

Symptoms:
- Telegram returns `Codex 응답 실패: Unauthorized.`
- Admin backend `/api/codex/chat` responds 401.

Likely causes:
- Dashboard password gate protects all `/api/*` and blocks bot server calls.
- Missing or mismatched `ADMIN_BACKEND_BOT_TOKEN` between services.

Fix direction:
- Keep browser auth gate.
- Add explicit bot-token auth path only for `/api/codex/chat`.
- Set same `ADMIN_BACKEND_BOT_TOKEN` on both `lx-agent` and `admin-dashboard`.

## 3) `Missing scopes: model.request`

Symptoms:
- Codex call fails despite OAuth success.

Likely causes:
- Account token not valid for OpenAI `/v1/chat/completions` model.request scope.

Fix direction:
- Retry via ChatGPT Codex responses endpoint when this specific error appears.
- Keep primary path unchanged for accounts where direct API works.

## 4) English-titled courses missing in selectors

Symptoms:
- `/assignments` selector shows only Korean semester-coded courses.

Likely causes:
- Current-term inference depends only on text like `2026-1` in name/course code.
- English titles in same term do not include that token.

Fix direction:
- Determine current term by matched courses, then expand to all courses sharing dominant `enrollment_term_id`.
- Add regression tests.
