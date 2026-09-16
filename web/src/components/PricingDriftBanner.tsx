import type { PricingDriftSnapshot } from "../api";

export function PricingDriftBanner({ snap }: { snap: PricingDriftSnapshot | null }) {
  if (!snap?.enabled || !snap.drifts?.length) return null;
  const n = snap.drifts.length;
  return (
    <div className="tier-card" role="status" data-tone="info" data-testid="pricing-drift-banner">
      <h2 className="tier-card-title">
        {n} {n === 1 ? "model differs" : "models differ"} from published rates
      </h2>
      <p className="tier-card-desc">
        Spend figures still use Nexus&apos;s static table. Confirm the vendor
        pricing page and update <code>pricing.go</code> — already recorded
        calls are not rewritten.
      </p>
    </div>
  );
}
