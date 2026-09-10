import { SectionLayout } from "./SectionLayout";
import { OBSERVABILITY_TABS } from "../nav/config";

export function ObservabilityLayout() {
  return (
    <SectionLayout
      title="Observability"
      subtitle="Dashboard, traces, spend, connectors, and MCP execution logs."
      tabs={OBSERVABILITY_TABS}
    />
  );
}
