import type { AuthConfig, Credential, VirtualKey } from "../api";

export type SetupStepId =
  | "cors"
  | "dashboard_auth"
  | "inference_auth"
  | "provider_key"
  | "virtual_key";

export type SetupStep = {
  id: SetupStepId;
  section: "security" | "provider";
  title: string;
  detail: string;
  done: boolean;
  to?: string;
};

export function gatewayBase(cfg?: AuthConfig | null): string {
  const explicit = cfg?.gateway_url?.replace(/\/$/, "");
  if (explicit) return explicit;
  if (typeof window === "undefined") return "http://localhost:8080";
  if (cfg?.local_mode) {
    return `${window.location.protocol}//${window.location.hostname}:8080`;
  }
  return window.location.origin;
}

export function buildSetupSteps(input: {
  cfg?: AuthConfig | null;
  credentials: Credential[];
  keys: VirtualKey[];
}): SetupStep[] {
  const keyMode = input.cfg?.key_mode || "strict_byok";
  const corsDone = Boolean(input.cfg?.local_mode || input.cfg?.cors_configured);
  return [
    {
      id: "cors",
      section: "security",
      title: "Restrict CORS origins",
      detail: corsDone
        ? input.cfg?.cors_configured
          ? "Cross-origin console hosts are on the allowlist."
          : "Same-origin only — the default. Set NEXUS_PUBLIC_WEB_ORIGINS if the SPA is on another host."
        : "Empty allowlist means same-origin only. Confirm that matches how you serve the console.",
      done: true,
      to: "/docs/configuration",
    },
    {
      id: "dashboard_auth",
      section: "security",
      title: "Set up dashboard auth",
      detail: input.cfg?.sso_enabled
        ? "SSO is on. Session cookies already gate the console."
        : "You are signed in. Invite others from Users, or wire OIDC for SSO.",
      done: true,
      to: "/users",
    },
    {
      id: "inference_auth",
      section: "security",
      title: "Enforce auth on inference",
      detail:
        keyMode === "shared"
          ? "Gateway is in shared-key mode. Switch to strict_byok so callers must present a virtual key."
          : `Gateway key mode is ${keyMode}. API callers need a virtual key.`,
      done: keyMode !== "shared",
      to: "/keys",
    },
    {
      id: "provider_key",
      section: "provider",
      title: "Add a provider key",
      detail:
        input.credentials.length > 0
          ? `${input.credentials.length} credential${input.credentials.length === 1 ? "" : "s"} stored.`
          : "Encrypt an OpenAI, Anthropic, Gemini, Mistral, Grid, or Ollama key so BYOK models can route.",
      done: input.credentials.length > 0,
      to: "/credentials",
    },
    {
      id: "virtual_key",
      section: "provider",
      title: "Mint a virtual key",
      detail:
        input.keys.filter((k) => !k.revoked).length > 0
          ? "A virtual key is ready for SDKs and the Playground."
          : "Issue a nxs_live_ key. The secret is shown once — paste it into the snippet below.",
      done: input.keys.filter((k) => !k.revoked).length > 0,
      to: "/keys",
    },
  ];
}
