import { SectionLayout } from "./SectionLayout";
import { MCP_TABS } from "../nav/config";

export function McpLayout() {
  return (
    <SectionLayout
      title="MCP"
      subtitle="Register MCP servers and manage tool gateway settings."
      tabs={MCP_TABS}
    />
  );
}
