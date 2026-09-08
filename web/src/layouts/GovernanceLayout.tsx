import { SectionLayout } from "./SectionLayout";
import { GOVERNANCE_TABS } from "../nav/config";

export function GovernanceLayout() {
  return (
    <SectionLayout
      title="Governance"
      subtitle="Org membership and control-plane audit trail."
      tabs={GOVERNANCE_TABS}
    />
  );
}
