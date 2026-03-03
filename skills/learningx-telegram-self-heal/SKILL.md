---
name: learningx-telegram-self-heal
description: Diagnose and fix lx-agent Telegram + LearningX regressions (bot polling conflicts, auth failures, missing course listings, and Codex reply failures), then validate and deploy safely. Use when users report Telegram warnings/errors, missing assignments/files in selectors, OAuth/Codex backend failures, or Railway deployment/runtime drift.
---

# LearningX Telegram Self-Heal

Run a deterministic debug-and-fix loop for `lx-agent` Telegram + LearningX issues.

## Workflow

1. Collect evidence before editing code.
2. Match a known failure signature.
3. Apply the smallest safe fix.
4. Validate locally.
5. Deploy and verify production behavior.
6. Report root cause, fix, and residual risk.

## Step 1: Collect Evidence

Run these first:

```bash
git status --short --branch
rg -n "getUpdates failed|error 409|Unauthorized|Missing scopes|/api/codex/chat|assignments|semester|EnrollmentTermID" cmd apps internal
```

If Railway is involved:

```bash
railway status
railway service status
railway service logs lx-agent
railway service logs admin-dashboard
```

## Step 2: Failure Signature Mapping

Use `references/failure-signatures.md` and map symptoms to one target fix path.

Priority order:
1. Runtime blockers (crash, hard 4xx/5xx auth failures).
2. Data visibility bugs (missing courses/assignments/files).
3. UX regressions (selector/menu behavior).
4. Deployment drift (env mismatch between `lx-agent` and `admin-dashboard`).

## Step 3: Minimal Safe Fix Policy

- Edit only files directly tied to the signature.
- Keep behavior stable outside the failing path.
- Prefer additive guards over broad refactors.
- Do not revert unrelated user changes.
- For auth fixes, preserve browser gate and add explicit server-to-server auth for bot paths only.

## Step 4: Validate Locally

Run the bundled script:

```bash
skills/learningx-telegram-self-heal/scripts/verify_readiness.sh
```

It runs:
- `go test ./...`
- `bun run admin:typecheck`
- `bun run admin:build`

If any step fails, stop and fix before deploy.

## Step 5: Deploy + Production Verification

Deploy when requested:

```bash
railway up --service lx-agent --detach
railway up --service admin-dashboard --detach
```

Optional API verification when bot-token auth is used:

```bash
ADMIN_URL="https://admin-dashboard-production-da11.up.railway.app" \
ADMIN_BACKEND_BOT_TOKEN="..." \
skills/learningx-telegram-self-heal/scripts/verify_readiness.sh --api-check
```

Expected:
- without token: `/api/codex/chat` returns 401
- with token: `/api/codex/chat` returns 200 and `ok:true`

## Step 6: Closeout Format

Report in this order:
1. Root cause
2. Evidence
3. Fix (files changed)
4. Validation run
5. Deployment + production checks
6. Remaining risk

## Guardrails

- Never use destructive git commands unless explicitly requested.
- Do not claim production-ready without validation outputs.
- Keep commit history clean: squash related fix commits before final push when requested.
