export type MCPPresetCategory = "filesystem" | "search" | "dev" | "remote";

export type MCPPresetRuntime = "local" | "hosted";

export type MCPPreset = {
  id: string;
  label: string;
  description: string;
  category: MCPPresetCategory;
  /** local = stdio in the gateway process; hosted = remote HTTP. */
  runtime: MCPPresetRuntime;
  defaultName: string;
  specYaml: string;
  requiresEnv?: string[];
  requiresHeaders?: string[];
};

export const MCP_PRESET_CATEGORIES: { id: MCPPresetCategory | "all"; label: string }[] = [
  { id: "all", label: "All" },
  { id: "filesystem", label: "Filesystem" },
  { id: "search", label: "Search" },
  { id: "dev", label: "Developer" },
  { id: "remote", label: "Remote HTTP" },
];

export const MCP_PRESETS: MCPPreset[] = [
  {
    id: "filesystem",
    label: "Filesystem",
    description: "Read and write files under a sandbox directory. Runs stdio on the gateway host.",
    category: "filesystem",
    runtime: "local",
    defaultName: "filesystem",
    specYaml: `connection:
  type: stdio
  command: npx
  args:
    - -y
    - "@modelcontextprotocol/server-filesystem"
    - /tmp
timeout_ms: 60000
`,
  },
  {
    id: "fetch",
    label: "Fetch",
    description: "Retrieve web content via a local stdio MCP server (npx on the gateway host).",
    category: "search",
    runtime: "local",
    defaultName: "fetch",
    specYaml: `connection:
  type: stdio
  command: npx
  args:
    - -y
    - "@modelcontextprotocol/server-fetch"
timeout_ms: 60000
`,
  },
  {
    id: "github",
    label: "GitHub",
    description: "Repository and issue tools over GitHub's hosted MCP. Paste a PAT in Authorization.",
    category: "dev",
    runtime: "hosted",
    defaultName: "github",
    requiresHeaders: ["Authorization"],
    specYaml: `connection:
  type: http
  url: https://api.githubcopilot.com/mcp/
  headers:
    Authorization: "Bearer <GITHUB_PAT>"
timeout_ms: 90000
sticky_http: true
`,
  },
  {
    id: "context7",
    label: "Context7",
    description: "Library docs and code examples from a hosted MCP endpoint. No API key.",
    category: "dev",
    runtime: "hosted",
    defaultName: "context7",
    specYaml: `connection:
  type: http
  url: https://mcp.context7.com/mcp
timeout_ms: 60000
sticky_http: true
`,
  },
  {
    id: "brave-search",
    label: "Brave Search",
    description: "Web search via Brave Search API. Runs stdio on the gateway host.",
    category: "search",
    runtime: "local",
    defaultName: "brave-search",
    requiresEnv: ["BRAVE_API_KEY"],
    specYaml: `connection:
  type: stdio
  command: npx
  args:
    - -y
    - "@modelcontextprotocol/server-brave-search"
  env:
    BRAVE_API_KEY: "<paste-api-key>"
timeout_ms: 60000
`,
  },
  {
    id: "remote-http",
    label: "Remote HTTP MCP",
    description: "Connect to a remote MCP server over HTTP.",
    category: "remote",
    runtime: "hosted",
    defaultName: "remote-mcp",
    specYaml: `connection:
  type: http
  url: https://example.com/mcp
  headers:
    Authorization: "Bearer <token>"
timeout_ms: 60000
sticky_http: true
`,
  },
];

export function getMCPPreset(id: string): MCPPreset | undefined {
  return MCP_PRESETS.find((p) => p.id === id);
}

/** Inject org defaults when absent from preset YAML. */
export function applyMcpOrgDefaults(
  specYaml: string,
  defaults: { default_timeout_ms: number; default_sticky_http: boolean },
): string {
  let out = specYaml.trimEnd();
  if (!/^timeout_ms:/m.test(out)) {
    out += `\ntimeout_ms: ${defaults.default_timeout_ms}`;
  }
  if (!/^sticky_http:/m.test(out)) {
    out += `\nsticky_http: ${defaults.default_sticky_http}`;
  }
  return out + "\n";
}
