import { Link } from "react-router-dom";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";
import { TierCard } from "../components/TierCard";

interface Props {
  title: string;
  status: "evaluate" | "users" | "keys" | "credentials" | "audit" | "playground";
  hint?: string;
}

const COPY: Record<Props["status"], { blurb: string; eyebrow: string }> = {
  evaluate: {
    blurb: "Heuristics, SLM judge, remote eval, and routing weights — all editable from one place.",
    eyebrow: "Admin · Eval & routing",
  },
  users: {
    blurb: "Members, roles, enforce-limits toggles, and per-user quality panels.",
    eyebrow: "Admin · Members",
  },
  keys: {
    blurb: "Org-wide virtual keys with rotate and copy curl.",
    eyebrow: "Workspace · API keys",
  },
  credentials: {
    blurb: "BYOK provider secrets — encrypted with the org master key.",
    eyebrow: "Workspace · Provider keys",
  },
  audit: {
    blurb: "Control-plane change log. Login, key create, credential rotate, and more.",
    eyebrow: "Admin · Audit",
  },
  playground: {
    blurb: "A one-shot chat panel using your own virtual key, with stream view.",
    eyebrow: "Workspace · Playground",
  },
};

export function Placeholder({ title, status, hint }: Props) {
  const meta = COPY[status];
  return (
    <div className="placeholder-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> {meta.eyebrow}
          </div>
          <h1 className="page-title">
            <GradientText as="span">{title}</GradientText>
          </h1>
          <p className="page-sub">{meta.blurb}</p>
          {hint && <p className="page-sub hint">Hint: {hint}</p>}
        </div>
      </header>

      <section className="placeholder-grid">
        <TierCard
          eyebrow="Next milestone"
          title={title + " · v0.6"}
          metric="Soon"
          description="The new shell ships this page next; the legacy tab still lives at /legacy for now."
          glow="violet"
          accent="#a855f7"
          ctaLabel={
            <Link to="/legacy">
              Open legacy tab <Icon.arrowRight size={14} />
            </Link>
          }
        />
        <TierCard
          eyebrow="Migration"
          title="Reused building blocks"
          metric="Drawable"
          description="DataTable, Drawer, Chips, StatusPill, and Theme tokens are ready to compose this view."
          glow="cyan"
          accent="#22d3ee"
        />
        <TierCard
          eyebrow="API surface"
          title="Same endpoints"
          metric="No change"
          description="Routing Stats, Eval Config PATCH, Audit, /api/me/keys, /api/users — all already wired."
          glow="pink"
          accent="#ec4899"
        />
      </section>
    </div>
  );
}
