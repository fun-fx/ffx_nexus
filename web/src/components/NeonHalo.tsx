import type { CSSProperties, ReactNode } from "react";

type Glow = "pink" | "cyan" | "violet" | "soft";

interface Props {
  glow?: Glow;
  hover?: boolean;
  className?: string;
  style?: CSSProperties;
  children: ReactNode;
}

const glowMap: Record<Glow, string> = {
  pink: "var(--glow-pink)",
  cyan: "var(--glow-cyan)",
  violet: "var(--glow-violet)",
  soft: "var(--glow-soft)",
};

/**
 * Wraps content with a CSS box-shadow halo. Hover boosts the halo; keyboard
 * focus pins a stable accessible halo so the neon styling still satisfies
 * focus-visible rules without hiding the focus ring.
 */
export function NeonHalo({
  glow = "violet",
  hover = true,
  className,
  style,
  children,
}: Props) {
  const baseShadow = glowMap[glow];
  const hoverShadow = baseShadow.replace(/0\.\d+\)$/, "0.7)");

  return (
    <div
      className={`neon-halo ${hover ? "is-hoverable" : ""} ${className ?? ""}`.trim()}
      style={
        {
          "--halo": baseShadow,
          "--halo-hover": hoverShadow,
          ...style,
        } as CSSProperties
      }
    >
      {children}
    </div>
  );
}
