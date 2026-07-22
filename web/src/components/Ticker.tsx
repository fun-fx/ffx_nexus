import { useEffect, useState } from "react";

interface Props {
  items: string[];
  className?: string;
}

const prefersReducedMotion = () =>
  typeof window !== "undefined" &&
  window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;

/**
 * Animates horizontally; duplicates the item list and translates left.
 * Respects prefers-reduced-motion by fading a tinted background gradient
 * only instead of motion.
 */
export function Ticker({ items, className }: Props) {
  const [reduced, setReduced] = useState(prefersReducedMotion());
  useEffect(() => {
    const mq = window.matchMedia?.("(prefers-reduced-motion: reduce)");
    if (!mq) return;
    const cb = (e: MediaQueryListEvent) => setReduced(e.matches);
    mq.addEventListener?.("change", cb);
    return () => mq.removeEventListener?.("change", cb);
  }, []);

  const list = [...items, ...items];

  return (
    <div
      className={`ticker ${reduced ? "is-static" : ""} ${className ?? ""}`.trim()}
      role="status"
      aria-live="polite"
      aria-label="Live workload summary"
    >
      <div className="ticker-track">
        {list.map((text, i) => (
          <span className="ticker-item" key={i}>
            {text}
            <span className="ticker-divider" aria-hidden="true">
              ◆
            </span>
          </span>
        ))}
      </div>
    </div>
  );
}
