/**
 * Pill-shaped on/off toggle with a visible label.
 *
 * Used everywhere a row needs an "in-UI on/off" affordance — heuristics
 * (PII / Completeness / SLM judge / Remote eval), Eval profiles, and any
 * future toggle-where-it-fits surface. Single shape, single size, single
 * font metric regardless of state: the cell itself never shifts.
 *
 * Visual states:
 *   on  — green pill   (#1f9d55) with "on"  text
 *   off — red pill     (#dc2626) with "off" text
 *
 * Click, Space, or Enter flip the value. Disabled while the row is mid
 * mutation so the operator doesn't double-fire an in-flight PATCH.
 */
export function LabelToggle({
  checked,
  disabled,
  onChange,
  label,
  "aria-disabled": ariaDisabled,
}: {
  checked: boolean;
  disabled?: boolean;
  onChange: (next: boolean) => void;
  label: string;
  /** External hint that the toggle should LOOK disabled even if the click
   * path itself early-returns on no-op (useful when the parent owns the
   * interactive flag but the cell is a visual signal — e.g. SLM judge). */
  "aria-disabled"?: boolean;
}) {
  const isDisabled = !!disabled || !!ariaDisabled;
  return (
    <span
      className={
        `label-toggle${checked ? " label-toggle-on" : " label-toggle-off"}`
      }
      role="switch"
      aria-checked={checked}
      aria-label={label}
      aria-disabled={isDisabled || undefined}
      tabIndex={isDisabled ? -1 : 0}
      onClick={() => {
        if (isDisabled) return;
        onChange(!checked);
      }}
      onKeyDown={(e) => {
        if (isDisabled) return;
        if (e.key === " " || e.key === "Enter") {
          e.preventDefault();
          onChange(!checked);
        }
      }}
    >
      {checked ? "on" : "off"}
    </span>
  );
}
