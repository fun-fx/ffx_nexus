import { SectionLayout } from "./SectionLayout";
import { DEVELOP_TABS } from "../nav/config";

export function DevelopLayout() {
  return (
    <SectionLayout
      title="Develop"
      subtitle="Playground and in-console documentation."
      tabs={DEVELOP_TABS}
    />
  );
}
