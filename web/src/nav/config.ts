import type { Icon } from "../components/icons";

export type NavIcon = keyof typeof Icon;

export type NavLinkItem = {
  to: string;
  label: string;
  icon?: NavIcon;
  badge?: "soon";
  adminOnly?: boolean;
  /** Match nested routes (e.g. /gateway/routing/foo). */
  end?: boolean;
};

export type NavGroupDef = {
  id: string;
  label: string;
  icon: NavIcon;
  adminOnly?: boolean;
  /** Single top-level link (no sub-items). */
  to?: string;
  items?: NavLinkItem[];
};

export const NAV_GROUPS: NavGroupDef[] = [
  {
    id: "overview",
    label: "Overview",
    icon: "grid",
    to: "/",
  },
  {
    id: "observability",
    label: "Observability",
    icon: "activity",
    items: [
      { to: "/observability/dashboard", label: "Dashboard", icon: "grid" },
      { to: "/observability/traces", label: "LLM Traces", icon: "chart" },
      { to: "/observability/spend", label: "Spend", icon: "wallet" },
      { to: "/observability/connectors", label: "Connectors", icon: "activity" },
      { to: "/observability/mcp-logs", label: "MCP Logs", icon: "list" },
    ],
  },
  {
    id: "gateway",
    label: "Gateway",
    icon: "zap",
    items: [
      { to: "/gateway/routing", label: "Routing", icon: "zap", end: false },
      { to: "/gateway/providers", label: "Providers", icon: "shield" },
      { to: "/gateway/keys", label: "Virtual Keys", icon: "keys" },
      { to: "/gateway/guardrails", label: "Guardrails", icon: "shield", adminOnly: true },
      { to: "/gateway/cache", label: "Semantic Cache", icon: "zap", adminOnly: true },
      { to: "/gateway/alerting", label: "Alerting", icon: "activity", adminOnly: true },
    ],
  },
  {
    id: "mcp",
    label: "MCP",
    icon: "zap",
    items: [
      { to: "/mcp/registry", label: "Registry", icon: "zap" },
      { to: "/mcp/library", label: "Library", icon: "doc" },
      { to: "/mcp/settings", label: "Settings", icon: "shield", adminOnly: true },
    ],
  },
  {
    id: "eval",
    label: "Eval",
    icon: "sparkles",
    adminOnly: true,
    items: [
      { to: "/eval", label: "Overview", icon: "sparkles", end: true },
      { to: "/eval/profiles", label: "Profiles", icon: "list" },
      { to: "/eval/plugins", label: "Plugins", icon: "zap" },
      { to: "/eval/benchmarks", label: "Benchmarks", icon: "chart" },
      { to: "/eval/routing", label: "Routing Integration", icon: "zap" },
    ],
  },
  {
    id: "governance",
    label: "Governance",
    icon: "users",
    adminOnly: true,
    items: [
      { to: "/governance/users", label: "Users", icon: "users" },
      { to: "/governance/audit", label: "Audit", icon: "list" },
    ],
  },
  {
    id: "develop",
    label: "Develop",
    icon: "play",
    items: [
      { to: "/develop/playground", label: "Playground", icon: "play" },
      { to: "/develop/docs", label: "Docs", icon: "doc", end: false },
    ],
  },
];

export type SectionTab = {
  to: string;
  label: string;
  end?: boolean;
};

export const OBSERVABILITY_TABS: SectionTab[] = [
  { to: "/observability/dashboard", label: "Dashboard" },
  { to: "/observability/traces", label: "LLM Traces" },
  { to: "/observability/spend", label: "Spend" },
  { to: "/observability/connectors", label: "Connectors" },
  { to: "/observability/mcp-logs", label: "MCP Logs" },
];

export const GATEWAY_TABS: SectionTab[] = [
  { to: "/gateway/routing", label: "Routing", end: false },
  { to: "/gateway/providers", label: "Providers" },
  { to: "/gateway/keys", label: "Virtual Keys" },
  { to: "/gateway/guardrails", label: "Guardrails" },
  { to: "/gateway/cache", label: "Semantic Cache" },
  { to: "/gateway/alerting", label: "Alerting" },
];

export const MCP_TABS: SectionTab[] = [
  { to: "/mcp/registry", label: "Registry" },
  { to: "/mcp/library", label: "Library" },
  { to: "/mcp/settings", label: "Settings" },
];

export const EVAL_TABS: SectionTab[] = [
  { to: "/eval", label: "Overview", end: true },
  { to: "/eval/profiles", label: "Profiles" },
  { to: "/eval/plugins", label: "Plugins" },
  { to: "/eval/benchmarks", label: "Benchmarks" },
  { to: "/eval/routing", label: "Routing Integration" },
];

export const GOVERNANCE_TABS: SectionTab[] = [
  { to: "/governance/users", label: "Users" },
  { to: "/governance/audit", label: "Audit" },
];

export const DEVELOP_TABS: SectionTab[] = [
  { to: "/develop/playground", label: "Playground" },
  { to: "/develop/docs", label: "Docs", end: false },
];
