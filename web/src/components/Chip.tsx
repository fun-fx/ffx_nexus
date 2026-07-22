import type { ReactNode } from "react";

export type ChipTone = "neutral" | "ok" | "warn" | "err" | "info" | "accent";

interface Props {
  tone?: ChipTone;
  icon?: ReactNode;
  onClick?: () => void;
  onRemove?: () => void;
  active?: boolean;
  children: ReactNode;
}

export function Chip({ tone = "neutral", icon, onClick, onRemove, active, children }: Props) {
  const Tag = onClick ? "button" : "span";
  return (
    <Tag
      type={onClick ? "button" : undefined}
      onClick={onClick}
      className={
        "chip" +
        ` chip-${tone}` +
        (onClick ? " chip-clickable" : "") +
        (active ? " is-active" : "")
      }
      aria-pressed={onClick ? active : undefined}
    >
      {icon && <span className="chip-icon" aria-hidden="true">{icon}</span>}
      <span className="chip-label">{children}</span>
      {onRemove && (
        <button
          type="button"
          className="chip-remove"
          onClick={(e) => {
            e.stopPropagation();
            onRemove();
          }}
          aria-label="Remove"
        >
          ×
        </button>
      )}
    </Tag>
  );
}
