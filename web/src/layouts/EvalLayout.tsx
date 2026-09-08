import { SectionLayout } from "./SectionLayout";
import { EVAL_TABS } from "../nav/config";

export function EvalLayout() {
  return (
    <SectionLayout
      title="Eval"
      subtitle="Quality scoring, external plugins, benchmarks, and routing integration."
      tabs={EVAL_TABS}
    />
  );
}
