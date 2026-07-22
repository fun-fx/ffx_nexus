import type { CSSProperties, ReactNode } from "react";

interface Props {
  as?: "span" | "h1" | "h2" | "h3" | "div";
  className?: string;
  style?: CSSProperties;
  children: ReactNode;
}

/**
 * Renders text using --accent-grad, with subtle text-shadow halo.
 * Tracked tight for hero-style headings.
 */
export function GradientText({ as: Tag = "span", children, className, style }: Props) {
  const baseStyle: CSSProperties = {
    background: "var(--accent-grad)",
    WebkitBackgroundClip: "text",
    backgroundClip: "text",
    color: "transparent",
    letterSpacing: "-0.02em",
    fontWeight: 700,
    ...style,
  };
  return (
    <Tag className={className} style={baseStyle}>
      {children}
    </Tag>
  );
}
