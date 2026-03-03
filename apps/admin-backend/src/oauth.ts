import { randomBytes } from "node:crypto";

const CODEX_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann";
const CODEX_AUTHORIZE_URL = "https://auth.openai.com/oauth/authorize";
const CODEX_TOKEN_URL = "https://auth.openai.com/oauth/token";
const CODEX_REDIRECT_URI = "http://localhost:1455/auth/callback";
const CODEX_SCOPE = "openid profile email offline_access";

const pendingFlows = new Map<string, { verifier: string; createdAt: number }>();
const flowTTLms = 10 * 60 * 1000;

function base64urlEncode(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=/g, "");
}

export async function generatePKCE(): Promise<{ verifier: string; challenge: string }> {
  const verifierBytes = new Uint8Array(32);
  crypto.getRandomValues(verifierBytes);
  const verifier = base64urlEncode(verifierBytes);

  const data = new TextEncoder().encode(verifier);
  const hash = await crypto.subtle.digest("SHA-256", data);
  const challenge = base64urlEncode(new Uint8Array(hash));
  return { verifier, challenge };
}

function cleanupFlows(): void {
  const now = Date.now();
  for (const [state, entry] of pendingFlows.entries()) {
    if (now - entry.createdAt > flowTTLms) pendingFlows.delete(state);
  }
}

function extractOAuthParam(raw: string, key: "code" | "state" | "error" | "error_description"): string | undefined {
  const text = raw.trim();
  if (!text) return undefined;

  try {
    const parsed = new URL(text);
    const fromQuery = parsed.searchParams.get(key);
    if (fromQuery) return fromQuery;

    const hash = parsed.hash.startsWith("#") ? parsed.hash.slice(1) : parsed.hash;
    const fromHash = new URLSearchParams(hash).get(key);
    if (fromHash) return fromHash;
  } catch {
    // Continue with non-URL parsing below.
  }

  const stripped = text.replace(/^[?#]/, "");
  const fromParams = new URLSearchParams(stripped).get(key);
  if (fromParams) return fromParams;

  const re = new RegExp(`[?#&]${key}=([^&#\\s]+)`);
  const m = text.match(re);
  if (m?.[1]) {
    try {
      return decodeURIComponent(m[1]);
    } catch {
      return m[1];
    }
  }

  return undefined;
}

export async function startCodexOAuthFlow(): Promise<{ url: string; state: string }> {
  cleanupFlows();
  const { verifier, challenge } = await generatePKCE();
  const state = randomBytes(16).toString("hex");

  const url = new URL(CODEX_AUTHORIZE_URL);
  url.searchParams.set("response_type", "code");
  url.searchParams.set("client_id", CODEX_CLIENT_ID);
  url.searchParams.set("redirect_uri", CODEX_REDIRECT_URI);
  url.searchParams.set("scope", CODEX_SCOPE);
  url.searchParams.set("code_challenge", challenge);
  url.searchParams.set("code_challenge_method", "S256");
  url.searchParams.set("state", state);
  url.searchParams.set("prompt", "login");
  url.searchParams.set("max_age", "0");
  url.searchParams.set("id_token_add_organizations", "true");
  url.searchParams.set("codex_cli_simplified_flow", "true");
  url.searchParams.set("originator", "pi");

  pendingFlows.set(state, { verifier, createdAt: Date.now() });
  return { url: url.toString(), state };
}

export type ExchangeInput = {
  redirectUrl?: string;
  code?: string;
  state?: string;
};

export type ExchangeOutput = {
  access_token: string;
  refresh_token: string;
  id_token?: string;
  expires_at?: number;
  scope?: string;
};

export async function exchangeCodexCode(input: ExchangeInput): Promise<ExchangeOutput> {
  cleanupFlows();
  let code = input.code;
  let state = input.state;
  let oauthError = "";
  let oauthErrorDescription = "";

  if (input.redirectUrl) {
    code = extractOAuthParam(input.redirectUrl, "code") ?? code;
    state = extractOAuthParam(input.redirectUrl, "state") ?? state;
    oauthError = extractOAuthParam(input.redirectUrl, "error") ?? "";
    oauthErrorDescription = extractOAuthParam(input.redirectUrl, "error_description") ?? "";
  }

  if (oauthError) {
    const detail = oauthErrorDescription ? `: ${oauthErrorDescription}` : "";
    throw new Error(`OAuth callback error (${oauthError})${detail}`);
  }

  if (!code || !state) {
    throw new Error("Missing authorization code or state. Paste the full redirect URL.");
  }

  const entry = pendingFlows.get(state);
  if (!entry) {
    throw new Error("Invalid or expired OAuth state. Start login again.");
  }
  pendingFlows.delete(state);

  const tokenRes = await fetch(CODEX_TOKEN_URL, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      client_id: CODEX_CLIENT_ID,
      code,
      code_verifier: entry.verifier,
      redirect_uri: CODEX_REDIRECT_URI,
    }),
  });

  if (!tokenRes.ok) {
    const text = await tokenRes.text().catch(() => "");
    throw new Error(`Token exchange failed (${tokenRes.status}): ${text}`);
  }

  const tokenJson = (await tokenRes.json()) as {
    access_token?: string;
    refresh_token?: string;
    id_token?: string;
    expires_in?: number;
    scope?: string;
  };

  if (!tokenJson.access_token || !tokenJson.refresh_token) {
    throw new Error("Invalid token response: missing access_token or refresh_token");
  }

  return {
    access_token: tokenJson.access_token,
    refresh_token: tokenJson.refresh_token,
    id_token: tokenJson.id_token,
    expires_at: tokenJson.expires_in ? Date.now() + tokenJson.expires_in * 1000 : undefined,
    scope: typeof tokenJson.scope === "string" ? tokenJson.scope : undefined,
  };
}
