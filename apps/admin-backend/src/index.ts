import { clearCodexAccount, hasCodexAccount, loadCodexAccount, loadConfig, saveCodexAccount, saveConfig } from "./config-store";
import { exchangeCodexCode, startCodexOAuthFlow } from "./oauth";
import { createHash, timingSafeEqual } from "node:crypto";
import { stat } from "node:fs/promises";
import { extname, resolve } from "node:path";
import { Pool } from "pg";

type JsonRecord = Record<string, unknown>;

function corsHeaders(origin?: string | null): HeadersInit {
  return {
    "Access-Control-Allow-Origin": origin || "*",
    "Access-Control-Allow-Headers": "Content-Type, Authorization, X-Admin-Bot-Token",
    "Access-Control-Allow-Methods": "GET,POST,PUT,OPTIONS",
  };
}

function json(body: JsonRecord, init: ResponseInit = {}, origin?: string | null): Response {
  return new Response(JSON.stringify(body), {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...corsHeaders(origin),
      ...(init.headers || {}),
    },
  });
}

async function parseJson(req: Request): Promise<JsonRecord> {
  try {
    return (await req.json()) as JsonRecord;
  } catch {
    return {};
  }
}

const port = Number(process.env.PORT || process.env.ADMIN_BACKEND_PORT || 8787);
const frontendDist = resolve(process.cwd(), "apps/admin-frontend/dist");
const databaseURL = String(process.env.DATABASE_URL || "").trim();
const dashboardPassword = String(process.env.ADMIN_DASHBOARD_PASSWORD || "").trim();
const backendBotToken = String(process.env.ADMIN_BACKEND_BOT_TOKEN || "").trim();
const authEnabled = dashboardPassword.length > 0;
const authCookieName = "lx_admin_session";
const authCookieMaxAge = Number(process.env.ADMIN_DASHBOARD_SESSION_SECONDS || 60 * 60 * 24 * 14);
const pool = databaseURL ? new Pool({ connectionString: databaseURL }) : null;
let schemaReady = false;
let schemaInitPromise: Promise<void> | null = null;

type ChatRow = {
  chat_id: string;
  lang: string;
  bindings: number;
  subscriptions: number;
  sent_alerts: number;
  last_sent_at: string | null;
};

type CodexAccount = {
  access_token: string;
  refresh_token: string;
  id_token?: string;
  expires_at?: number;
  scope?: string;
};

type CodexTurn = {
  role: "user" | "assistant";
  content: string;
};

const codexConversations = new Map<string, CodexTurn[]>();
const maxConversationTurns = 12;
const codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses";

function hashText(input: string): string {
  return createHash("sha256").update(input).digest("hex");
}

const expectedSessionToken = authEnabled ? hashText(dashboardPassword) : "";

function secureStringEqual(a: string, b: string): boolean {
  const aa = Buffer.from(a);
  const bb = Buffer.from(b);
  if (aa.length !== bb.length) return false;
  return timingSafeEqual(aa, bb);
}

function getCookieValue(req: Request, key: string): string {
  const raw = req.headers.get("cookie");
  if (!raw) return "";
  const chunks = raw.split(";");
  for (const chunk of chunks) {
    const [k, ...v] = chunk.trim().split("=");
    if (k !== key) continue;
    return decodeURIComponent(v.join("="));
  }
  return "";
}

function isAuthenticated(req: Request): boolean {
  if (!authEnabled) return true;
  const token = getCookieValue(req, authCookieName);
  if (!token) return false;
  return secureStringEqual(token, expectedSessionToken);
}

function getBearerToken(req: Request): string {
  const auth = String(req.headers.get("authorization") || "").trim();
  if (!auth) return "";
  const match = auth.match(/^Bearer\s+(.+)$/i);
  return match ? String(match[1] || "").trim() : "";
}

function isBotAuthorized(req: Request, pathname: string): boolean {
  if (pathname !== "/api/codex/chat") return false;
  if (!backendBotToken) return false;

  const providedToken =
    String(req.headers.get("x-admin-bot-token") || "").trim() || getBearerToken(req);
  if (!providedToken) return false;
  return secureStringEqual(providedToken, backendBotToken);
}

function shouldUseSecureCookie(req: Request): boolean {
  const proto = (req.headers.get("x-forwarded-proto") || "").toLowerCase();
  if (proto === "https") return true;
  try {
    return new URL(req.url).protocol === "https:";
  } catch {
    return false;
  }
}

function sessionCookie(req: Request): string {
  const secure = shouldUseSecureCookie(req) ? "; Secure" : "";
  return `${authCookieName}=${encodeURIComponent(expectedSessionToken)}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${Math.max(60, authCookieMaxAge)}${secure}`;
}

function clearSessionCookie(req: Request): string {
  const secure = shouldUseSecureCookie(req) ? "; Secure" : "";
  return `${authCookieName}=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0${secure}`;
}

function passwordGateHtml(nextPath: string): string {
  const safeNext = JSON.stringify(nextPath || "/");
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Admin Login</title>
  <style>
    :root { color-scheme: light; }
    * { box-sizing: border-box; }
    body { margin: 0; font-family: ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, Helvetica, Arial; background: #f4f6f8; color: #0f172a; }
    .wrap { min-height: 100vh; display: grid; place-items: center; padding: 24px; }
    .card { width: 100%; max-width: 420px; background: white; border: 1px solid #e2e8f0; border-radius: 14px; padding: 22px; box-shadow: 0 8px 30px rgba(2, 6, 23, 0.06); }
    h1 { margin: 0 0 6px; font-size: 20px; }
    p { margin: 0 0 16px; color: #475569; font-size: 14px; }
    input { width: 100%; border: 1px solid #cbd5e1; border-radius: 10px; padding: 10px 12px; font-size: 15px; }
    button { margin-top: 12px; width: 100%; border: 0; border-radius: 10px; padding: 10px 12px; font-size: 15px; background: #0f172a; color: white; cursor: pointer; }
    button:disabled { opacity: 0.7; cursor: not-allowed; }
    .msg { margin-top: 10px; min-height: 18px; font-size: 13px; color: #b91c1c; white-space: pre-wrap; }
  </style>
</head>
<body>
  <div class="wrap">
    <div class="card">
      <h1>Password Required</h1>
      <p>This dashboard is protected.</p>
      <form id="form">
        <input id="password" type="password" autocomplete="current-password" placeholder="Enter password" required />
        <button id="submit" type="submit">Unlock</button>
      </form>
      <div class="msg" id="msg"></div>
    </div>
  </div>
  <script>
    const form = document.getElementById("form");
    const password = document.getElementById("password");
    const submit = document.getElementById("submit");
    const msg = document.getElementById("msg");
    const next = ${safeNext};
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      msg.textContent = "";
      submit.disabled = true;
      try {
        const res = await fetch("/api/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password: password.value })
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok || !data.ok) throw new Error(data.error || "Login failed");
        location.assign(next || "/");
      } catch (err) {
        msg.textContent = (err && err.message) ? err.message : String(err);
      } finally {
        submit.disabled = false;
      }
    });
  </script>
</body>
</html>`;
}

function isAuthExemptPath(pathname: string): boolean {
  return pathname === "/health" || pathname === "/api/auth/login" || pathname === "/api/auth/logout" || pathname === "/api/auth/status";
}

function normalizeCodexModel(modelId: string): string {
  const stripped = modelId.replace(/^openai-codex\//, "");
  if (stripped === "gpt-5.3-codex-spark") return "gpt-5.3-codex";
  return stripped || "gpt-5.3-codex";
}

function decodeJwtPayload(token: string): JsonRecord | null {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  try {
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
    const jsonText = Buffer.from(padded, "base64").toString("utf8");
    const parsed = JSON.parse(jsonText);
    if (!parsed || typeof parsed !== "object") return null;
    return parsed as JsonRecord;
  } catch {
    return null;
  }
}

function extractChatgptAccountId(accessToken: string): string {
  const payload = decodeJwtPayload(accessToken);
  if (!payload) return "";
  const authClaim = payload["https://api.openai.com/auth"];
  if (!authClaim || typeof authClaim !== "object") return "";
  const id = (authClaim as JsonRecord).chatgpt_account_id;
  return typeof id === "string" ? id.trim() : "";
}

function extractTextFromResponsesEnvelope(node: unknown): string {
  if (!node || typeof node !== "object") return "";
  const obj = node as JsonRecord;

  if (typeof obj.output_text === "string") {
    return obj.output_text.trim();
  }

  const output = obj.output;
  if (!Array.isArray(output)) return "";

  const chunks: string[] = [];
  for (const item of output) {
    if (!item || typeof item !== "object") continue;
    const content = (item as JsonRecord).content;
    if (!Array.isArray(content)) continue;
    for (const part of content) {
      if (!part || typeof part !== "object") continue;
      const text = (part as JsonRecord).text;
      if (typeof text === "string" && text.trim()) chunks.push(text.trim());
    }
  }
  return chunks.join("\n").trim();
}

async function readErrorBody(resp: Response): Promise<string> {
  const payload = (await resp.json().catch(() => null)) as
    | { error?: { message?: string }; message?: string; detail?: string }
    | null;
  const message = String(payload?.error?.message || payload?.message || payload?.detail || "").trim();
  if (message) return message;
  const text = String(await resp.text().catch(() => "")).trim();
  return text || `Codex request failed (${resp.status})`;
}

async function chatWithCodexResponses(input: {
  accessToken: string;
  model: string;
  systemPrompt: string;
  turns: CodexTurn[];
}): Promise<string> {
  const accountId = extractChatgptAccountId(input.accessToken);
  const responseInput = input.turns.map((turn) => ({
    role: turn.role,
    content: [{ type: "input_text", text: turn.content }],
  }));

  const resp = await fetch(codexResponsesURL, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${input.accessToken}`,
      "Content-Type": "application/json",
      accept: "text/event-stream",
      "OpenAI-Beta": "responses=experimental",
      originator: "pi",
      ...(accountId ? { "chatgpt-account-id": accountId } : {}),
    },
    body: JSON.stringify({
      model: input.model,
      instructions: input.systemPrompt,
      input: responseInput,
      store: false,
      stream: true,
    }),
  });

  if (!resp.ok) {
    throw new Error(await readErrorBody(resp));
  }

  if (!resp.body) {
    throw new Error("Codex response stream unavailable.");
  }

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let deltaText = "";
  let completedText = "";

  const processBlock = (block: string): void => {
    const dataLines = block
      .split("\n")
      .map((line) => line.trimEnd())
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).trimStart());
    if (dataLines.length === 0) return;
    const data = dataLines.join("\n").trim();
    if (!data || data === "[DONE]") return;

    let parsed: JsonRecord;
    try {
      parsed = JSON.parse(data) as JsonRecord;
    } catch {
      return;
    }

    if (parsed.type === "response.output_text.delta" && typeof parsed.delta === "string") {
      deltaText += parsed.delta;
      return;
    }

    if (parsed.type === "response.completed") {
      completedText = extractTextFromResponsesEnvelope(parsed.response);
      return;
    }

    if (!completedText) {
      completedText = extractTextFromResponsesEnvelope(parsed);
    }
  };

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let splitAt = buffer.indexOf("\n\n");
    while (splitAt >= 0) {
      const block = buffer.slice(0, splitAt);
      buffer = buffer.slice(splitAt + 2);
      processBlock(block);
      splitAt = buffer.indexOf("\n\n");
    }
  }
  buffer += decoder.decode();
  if (buffer.trim()) {
    processBlock(buffer);
  }

  const reply = String(deltaText || completedText).trim();
  if (!reply) {
    throw new Error("Codex returned an empty response.");
  }
  return reply;
}

function getConversation(chatId: string): CodexTurn[] {
  return codexConversations.get(chatId) ?? [];
}

function saveConversation(chatId: string, turns: CodexTurn[]): void {
  if (!chatId) return;
  const clipped = turns.slice(-maxConversationTurns);
  codexConversations.set(chatId, clipped);
}

async function chatWithCodex(input: { chatId: string; message: string; lang?: string }): Promise<string> {
  const account = await loadCodexAccountPersist();
  if (!account?.access_token) {
    throw new Error("Codex account is not linked. Login with ChatGPT in admin dashboard first.");
  }

  const config = await loadConfig();
  const model = normalizeCodexModel(config.defaultModel);
  const conversation = getConversation(input.chatId);
  const systemPrompt =
    input.lang === "en"
      ? "You are a concise Telegram assistant for university course support."
      : "당신은 대학 강의 지원용 텔레그램 도우미입니다. 짧고 정확하게 답하세요.";

  const messages: Array<{ role: "system" | "user" | "assistant"; content: string }> = [
    { role: "system", content: systemPrompt },
    ...conversation,
    { role: "user", content: input.message },
  ];

  const resp = await fetch("https://api.openai.com/v1/chat/completions", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${account.access_token}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      model,
      messages,
      temperature: 0.2,
    }),
  });

  const payload = (await resp.json().catch(() => ({}))) as {
    choices?: Array<{ message?: { content?: string } }>;
    error?: { message?: string };
  };

  let reply = "";

  if (!resp.ok) {
    const upstream = String(payload.error?.message || `Codex request failed (${resp.status})`);
    if (upstream.includes("Missing scopes: model.request")) {
      reply = await chatWithCodexResponses({
        accessToken: account.access_token,
        model,
        systemPrompt,
        turns: [...conversation, { role: "user", content: input.message }],
      });
    } else {
      throw new Error(upstream);
    }
  } else {
    reply = String(payload.choices?.[0]?.message?.content || "").trim();
  }

  if (!reply) {
    throw new Error("Codex returned an empty response.");
  }

  saveConversation(input.chatId, [...conversation, { role: "user", content: input.message }, { role: "assistant", content: reply }]);
  return reply;
}

async function ensureChatSchema(): Promise<void> {
  if (!pool || schemaReady) return;
  if (schemaInitPromise) return schemaInitPromise;

  schemaInitPromise = (async () => {
    const client = await pool.connect();
    try {
      await client.query("BEGIN");
      await client.query("SELECT pg_advisory_xact_lock(781234567890123)");
      await client.query(`
CREATE TABLE IF NOT EXISTS telegram_bindings (
  canvas_api_key TEXT PRIMARY KEY,
  chat_id TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`);
      await client.query(`
CREATE TABLE IF NOT EXISTS telegram_chat_settings (
  chat_id TEXT PRIMARY KEY,
  lang TEXT NOT NULL DEFAULT 'ko',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`);
      await client.query(`
CREATE TABLE IF NOT EXISTS telegram_chat_courses (
  chat_id TEXT NOT NULL,
  course_id INTEGER NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (chat_id, course_id)
)`);
      await client.query(`
CREATE TABLE IF NOT EXISTS telegram_sent_alerts (
  chat_id TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  alert_type TEXT NOT NULL,
  course_id INTEGER NULL,
  entity_id BIGINT NULL,
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
  sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (chat_id, dedupe_key)
)`);
      await client.query(`
CREATE TABLE IF NOT EXISTS admin_codex_accounts (
  id TEXT PRIMARY KEY,
  account JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`);
      await client.query("COMMIT");
      schemaReady = true;
    } catch (err) {
      await client.query("ROLLBACK");
      throw err;
    } finally {
      client.release();
      schemaInitPromise = null;
    }
  })();

  return schemaInitPromise;
}

async function listChats(): Promise<ChatRow[]> {
  if (!pool) return [];
  await ensureChatSchema();

  const result = await pool.query<ChatRow>(`
SELECT
  c.chat_id,
  COALESCE(s.lang, 'ko') AS lang,
  COALESCE(b.bindings, 0) AS bindings,
  COALESCE(cc.subscriptions, 0) AS subscriptions,
  COALESCE(sa.sent_alerts, 0) AS sent_alerts,
  sa.last_sent_at
FROM (
  SELECT chat_id FROM telegram_bindings
  UNION
  SELECT chat_id FROM telegram_chat_settings
  UNION
  SELECT chat_id FROM telegram_chat_courses
  UNION
  SELECT chat_id FROM telegram_sent_alerts
) c
LEFT JOIN telegram_chat_settings s ON s.chat_id = c.chat_id
LEFT JOIN (
  SELECT chat_id, COUNT(*)::int AS bindings
  FROM telegram_bindings
  GROUP BY chat_id
) b ON b.chat_id = c.chat_id
LEFT JOIN (
  SELECT chat_id, COUNT(*)::int AS subscriptions
  FROM telegram_chat_courses
  GROUP BY chat_id
) cc ON cc.chat_id = c.chat_id
LEFT JOIN (
  SELECT chat_id, COUNT(*)::int AS sent_alerts, MAX(sent_at)::text AS last_sent_at
  FROM telegram_sent_alerts
  GROUP BY chat_id
) sa ON sa.chat_id = c.chat_id
ORDER BY c.chat_id
`);
  return result.rows;
}

function normalizeCodexAccount(input: unknown): CodexAccount | null {
  const parsed = input as Partial<CodexAccount> | null;
  if (!parsed || typeof parsed.access_token !== "string" || typeof parsed.refresh_token !== "string") {
    return null;
  }
  return {
    access_token: parsed.access_token,
    refresh_token: parsed.refresh_token,
    id_token: typeof parsed.id_token === "string" ? parsed.id_token : undefined,
    expires_at: typeof parsed.expires_at === "number" ? parsed.expires_at : undefined,
    scope: typeof parsed.scope === "string" ? parsed.scope : undefined,
  };
}

async function saveCodexAccountPersist(account: CodexAccount): Promise<void> {
  await saveCodexAccount(account);
  if (!pool) return;

  await ensureChatSchema();
  await pool.query(
    `
INSERT INTO admin_codex_accounts (id, account, updated_at)
VALUES ('default', $1::jsonb, NOW())
ON CONFLICT (id)
DO UPDATE SET
  account = EXCLUDED.account,
  updated_at = NOW()
`,
    [JSON.stringify(account)]
  );
}

async function loadCodexAccountPersist(): Promise<CodexAccount | null> {
  if (pool) {
    await ensureChatSchema();
    const result = await pool.query<{ account: unknown }>(
      "SELECT account FROM admin_codex_accounts WHERE id = 'default' LIMIT 1"
    );
    if (result.rowCount && result.rows[0]) {
      const fromDB = normalizeCodexAccount(result.rows[0].account);
      if (fromDB) return fromDB;
    }
  }
  const fromFile = await loadCodexAccount();
  return normalizeCodexAccount(fromFile);
}

async function clearCodexAccountPersist(): Promise<void> {
  await clearCodexAccount();
  codexConversations.clear();
  if (!pool) return;

  await ensureChatSchema();
  await pool.query("DELETE FROM admin_codex_accounts WHERE id = 'default'");
}

async function hasCodexAccountPersist(): Promise<boolean> {
  if (pool) {
    await ensureChatSchema();
    const result = await pool.query<{ exists: boolean }>(
      "SELECT EXISTS (SELECT 1 FROM admin_codex_accounts WHERE id = 'default') AS exists"
    );
    if (result.rows[0]?.exists) return true;
  }
  return hasCodexAccount();
}

function contentTypeFor(pathname: string): string | undefined {
  switch (extname(pathname).toLowerCase()) {
    case ".html":
      return "text/html; charset=utf-8";
    case ".js":
      return "application/javascript; charset=utf-8";
    case ".css":
      return "text/css; charset=utf-8";
    case ".json":
      return "application/json; charset=utf-8";
    case ".svg":
      return "image/svg+xml";
    case ".png":
      return "image/png";
    case ".jpg":
    case ".jpeg":
      return "image/jpeg";
    case ".webp":
      return "image/webp";
    default:
      return undefined;
  }
}

async function serveFrontend(pathname: string): Promise<Response | null> {
  if (pathname.startsWith("/api/")) return null;

  let rel = pathname;
  if (rel === "/" || rel === "") rel = "/index.html";
  const candidate = resolve(frontendDist, "." + rel);
  if (!candidate.startsWith(frontendDist)) {
    return new Response("forbidden", { status: 403 });
  }

  try {
    const s = await stat(candidate);
    if (s.isFile()) {
      const ct = contentTypeFor(candidate);
      return new Response(Bun.file(candidate), ct ? { headers: { "Content-Type": ct } } : undefined);
    }
  } catch {
    // Fall through to SPA index fallback.
  }

  const indexPath = resolve(frontendDist, "index.html");
  try {
    const s = await stat(indexPath);
    if (s.isFile()) {
      return new Response(Bun.file(indexPath), { headers: { "Content-Type": "text/html; charset=utf-8" } });
    }
  } catch {
    return null;
  }
  return null;
}

Bun.serve({
  port,
  async fetch(req) {
    const origin = req.headers.get("origin");
    const url = new URL(req.url);

    if (req.method === "OPTIONS") {
      return new Response(null, { status: 204, headers: corsHeaders(origin) });
    }

    if (url.pathname === "/health") {
      return json({ ok: true }, {}, origin);
    }

    if (url.pathname === "/api/auth/status" && req.method === "GET") {
      return json(
        {
          ok: true,
          enabled: authEnabled,
          authenticated: isAuthenticated(req),
        },
        {},
        origin
      );
    }

    if (url.pathname === "/api/auth/login" && req.method === "POST") {
      if (!authEnabled) {
        return json({ ok: true, enabled: false }, {}, origin);
      }
      const body = await parseJson(req);
      const password = String(body.password || "");
      if (!secureStringEqual(password, dashboardPassword)) {
        return json({ ok: false, error: "Invalid password." }, { status: 401 }, origin);
      }
      return json({ ok: true }, { headers: { "Set-Cookie": sessionCookie(req), "Cache-Control": "no-store" } }, origin);
    }

    if (url.pathname === "/api/auth/logout" && req.method === "POST") {
      return json({ ok: true }, { headers: { "Set-Cookie": clearSessionCookie(req), "Cache-Control": "no-store" } }, origin);
    }

    if (authEnabled && !isAuthExemptPath(url.pathname) && !isAuthenticated(req) && !isBotAuthorized(req, url.pathname)) {
      if (url.pathname.startsWith("/api/")) {
        return json({ ok: false, error: "Unauthorized." }, { status: 401 }, origin);
      }
      if (req.method === "GET" || req.method === "HEAD") {
        const next = `${url.pathname}${url.search}`;
        return new Response(passwordGateHtml(next || "/"), {
          status: 401,
          headers: {
            "Content-Type": "text/html; charset=utf-8",
            "Cache-Control": "no-store",
          },
        });
      }
      return new Response("unauthorized", { status: 401 });
    }

    if (url.pathname === "/api/config" && req.method === "GET") {
      const config = await loadConfig();
      const linked = await hasCodexAccountPersist();
      return json({ config, linked }, {}, origin);
    }

    if (url.pathname === "/api/chat-ids" && req.method === "GET") {
      try {
        const chats = await listChats();
        return json({ ok: true, chats, databaseConnected: Boolean(pool) }, {}, origin);
      } catch (err) {
        return json(
          { ok: false, error: err instanceof Error ? err.message : String(err), databaseConnected: Boolean(pool) },
          { status: 500 },
          origin
        );
      }
    }

    if (url.pathname === "/api/config" && req.method === "PUT") {
      const body = await parseJson(req);
      const current = await loadConfig();
      const nextModel = String(body.defaultModel || current.defaultModel);
      if (!["openai-codex/gpt-5.3-codex-spark", "openai-codex/gpt-5.3-codex"].includes(nextModel)) {
        return json({ ok: false, error: "unsupported model" }, { status: 400 }, origin);
      }

      const next = {
        ...current,
        defaultModel: nextModel as "openai-codex/gpt-5.3-codex-spark" | "openai-codex/gpt-5.3-codex",
      };
      await saveConfig(next);
      return json({ ok: true, config: next }, {}, origin);
    }

    if (url.pathname === "/api/providers/openai-codex/oauth/start" && req.method === "POST") {
      try {
        const result = await startCodexOAuthFlow();
        return json({ ok: true, ...result }, {}, origin);
      } catch (err) {
        return json({ ok: false, error: err instanceof Error ? err.message : String(err) }, { status: 500 }, origin);
      }
    }

    if (url.pathname === "/api/providers/openai-codex/oauth/callback" && req.method === "POST") {
      try {
        const body = await parseJson(req);
        const token = await exchangeCodexCode({
          redirectUrl: typeof body.redirectUrl === "string" ? body.redirectUrl : undefined,
          code: typeof body.code === "string" ? body.code : undefined,
          state: typeof body.state === "string" ? body.state : undefined,
        });

        await saveCodexAccountPersist(token as CodexAccount);
        const current = await loadConfig();
        await saveConfig({
          ...current,
          codex: {
            ...current.codex,
            enabled: true,
            linkedAt: new Date().toISOString(),
          },
        });

        return json({ ok: true }, {}, origin);
      } catch (err) {
        return json({ ok: false, error: err instanceof Error ? err.message : String(err) }, { status: 400 }, origin);
      }
    }

    if (url.pathname === "/api/providers/openai-codex/oauth/remove" && req.method === "POST") {
      try {
        await clearCodexAccountPersist();
        const current = await loadConfig();
        await saveConfig({
          ...current,
          codex: {
            ...current.codex,
            enabled: false,
            linkedAt: undefined,
          },
        });
        return json({ ok: true }, {}, origin);
      } catch (err) {
        return json({ ok: false, error: err instanceof Error ? err.message : String(err) }, { status: 500 }, origin);
      }
    }

    if (url.pathname === "/api/codex/chat" && req.method === "POST") {
      try {
        const body = await parseJson(req);
        const message = String(body.message || "").trim();
        const chatId = String(body.chatId || "default").trim() || "default";
        const lang = typeof body.lang === "string" ? body.lang : undefined;
        if (!message) {
          return json({ ok: false, error: "message is required" }, { status: 400 }, origin);
        }

        const reply = await chatWithCodex({ chatId, message, lang });
        return json({ ok: true, reply }, {}, origin);
      } catch (err) {
        return json({ ok: false, error: err instanceof Error ? err.message : String(err) }, { status: 500 }, origin);
      }
    }

    if (req.method === "GET" || req.method === "HEAD") {
      const staticResp = await serveFrontend(url.pathname);
      if (staticResp) return staticResp;
    }

    return json({ ok: false, error: "not found" }, { status: 404 }, origin);
  },
});

console.log(`admin-backend listening on http://localhost:${port}`);
