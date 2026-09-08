import { SectionLayout } from "./SectionLayout";
import { OBSERVABILITY_TABS } from "../nav/config";

export function ObservabilityLayout() {
  return (
    <SectionLayout
      title="Observability"
      subtitle="Traces, spend, connectors, and MCP execution logs."
      tabs={OBSERVABILITY_TABS}
    />
  );
}
