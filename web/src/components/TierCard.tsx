import type { ReactNode } from "react";

interface Props {
  eyebrow?: ReactNode;
  title: ReactNode;
  metric?: ReactNode;
  description?: ReactNode;
  glow?: "pink" | "cyan" | "violet";
  ctaLabel?: ReactNode;
  onClick?: () => void;
  accent?: string;
}

/**
 * Grid-style tier card — large headline + metric + description + haloed CTA.
 * Used directly for routing tier previews and Stack:premium look.
 */
export function TierCard({
  eyebrow,
  title,
  metric,
  description,
  glow = "violet",
  ctaLabel,
  onClick,
  accent,
}: Props) {
  const halo = glow === "pink" ? "var(--glow-pink)" :
    glow === "cyan" ? "var(--glow-cyan)" : "var(--glow-violet)";
  return (
    <div
      className="tier-card"
      role={onClick ? "button" : undefined}
      tabIndex={onClick ? 0 : undefined}
      onClick={onClick}
      onKeyDown={(e) => {
        if (!onClick) return;
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      }}
      style={
        {
          "--tier-glow": halo,
          "--tier-accent": accent ?? "var(--accent-3)",
        } as React.CSSProperties
      }
    >
      <div className="tier-card-accent" aria-hidden="true" />
      <div className="tier-card-body">
        {eyebrow && <div className="tier-card-eyebrow">{eyebrow}</div>}
        <div className="tier-card-title">{title}</div>
        {metric && <div className="tier-card-metric">{metric}</div>}
        {description && <div className="tier-card-desc">{description}</div>}
        {ctaLabel && (
          <div className="tier-card-cta" aria-hidden={!onClick}>
            {ctaLabel}
            <span className="arrow">→</span>
          </div>
        )}
      </div>
    </div>
  );
}
