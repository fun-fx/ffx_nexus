import { ComingSoonPage } from "../components/ComingSoonPage";

export function GuardrailsPlaceholder() {
  return (
    <ComingSoonPage
      title="Guardrails"
      description="Configure input/output guardrails (PII block, JSON validation, max input length) from the console. Today these knobs live in Helm env (NEXUS_GUARDRAILS_*)."
      docsHref="/develop/docs/configuration"
    />
  );
}

export function SemanticCachePlaceholder() {
  return (
    <ComingSoonPage
      title="Semantic Cache"
      description="Tune semantic cache threshold, TTL, and max entries. Backend support exists via NEXUS_SEMANTIC_CACHE_* — console editor coming soon."
      docsHref="/develop/docs/configuration"
    />
  );
}

export function AlertingPlaceholder() {
  return (
    <ComingSoonPage
      title="Alerting"
      description="Failover webhooks and spend alert channels. NEXUS_FAILOVER_WEBHOOK is configurable today; a full alerting UI is on the roadmap."
      docsHref="/develop/docs/configuration"
    />
  );
}

export function McpLibraryPlaceholder() {
  return (
    <ComingSoonPage
      title="MCP Library"
      description="Curated MCP server installs (Bifrost MCP Library parity). Register custom servers on the Registry tab today."
    />
  );
}

export function McpSettingsPlaceholder() {
  return (
    <ComingSoonPage
      title="MCP Settings"
      description="Global MCP gateway settings, OAuth grants, and session management."
    />
  );
}
