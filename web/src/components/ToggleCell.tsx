/**
 * Single on/off switch cell. Shared across Eval rules (PII / Completeness),
 * Eval profiles, and any other "in-row enabled flag" use case.
 *
 * Same shape and size regardless of state — the off state keeps the
 * muted panel tone and the on state slides the thumb across the accent
 * gradient. No labels, no alignment tricks: the cell *is* the toggle.
 *
 * Click, Space, or Enter all flip the value. Disabled while the row is
 * mid-mutation.
 */
export function ToggleCell({
  checked,
  disabled,
  onChange,
  label,
}: {
  checked: boolean;
  disabled?: boolean;
  onChange: (next: boolean) => void;
  label: string;
}) {
  return (
    <span
      className={`toggle-cell${checked ? " toggle-cell-on" : ""}`}
      role="switch"
      aria-checked={checked}
      aria-label={label}
      tabIndex={disabled ? -1 : 0}
      onClick={() => {
        if (!disabled) onChange(!checked);
      }}
      onKeyDown={(e) => {
        if (disabled) return;
        if (e.key === " " || e.key === "Enter") {
          e.preventDefault();
          onChange(!checked);
        }
      }}
    >
      <span className="toggle-cell-track">
        <span className="toggle-cell-thumb" />
      </span>
    </span>
  );
}
