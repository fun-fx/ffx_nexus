import type { ReactNode } from "react";

export type ChipTone = "neutral" | "ok" | "warn" | "err" | "info" | "accent";

interface Props {
  tone?: ChipTone;
  icon?: ReactNode;
  onClick?: () => void;
  onRemove?: () => void;
  active?: boolean;
  children: ReactNode;
  // Test hooks: callers occasionally need a stable selector
  // for `getByTestId` without rewriting Chip itself; aliasing
  // to "data-testid" accepts either spelling so existing call
  // sites in Eval.tsx keep working.
  "data-testid"?: string;
}

export function Chip({ tone = "neutral", icon, onClick, onRemove, active, children, ...rest }: Props) {
  const Tag = onClick ? "button" : "span";
  return (
    <Tag
      type={onClick ? "button" : undefined}
      onClick={onClick}
      data-testid={rest["data-testid"]}
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
