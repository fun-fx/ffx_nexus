import { SectionLayout } from "./SectionLayout";
import { GATEWAY_TABS } from "../nav/config";

export function GatewayLayout() {
  return (
    <SectionLayout
      title="Gateway"
      subtitle="Routing, providers, virtual keys, and traffic policy."
      tabs={GATEWAY_TABS}
    />
  );
}
