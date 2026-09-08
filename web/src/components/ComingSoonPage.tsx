import { Link } from "react-router-dom";
import { GradientText } from "./GradientText";

export function ComingSoonPage({
  title,
  description,
  docsHref,
}: {
  title: string;
  description: string;
  docsHref?: string;
}) {
  return (
    <div className="placeholder-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Coming soon
          </div>
          <h2 className="page-title">
            <GradientText as="span">{title}</GradientText>
          </h2>
          <p className="page-sub">{description}</p>
        </div>
      </header>
      <div className="panel" style={{ padding: "1.25rem" }}>
        <p className="muted">
          This surface is on the Bifrost-parity roadmap. Backend hooks may
          already exist via Helm env — the console UI is not wired yet.
        </p>
        {docsHref ? (
          <p style={{ marginTop: "0.75rem" }}>
            <Link to={docsHref} className="btn ghost">
              Read docs
            </Link>
          </p>
        ) : null}
      </div>
    </div>
  );
}
