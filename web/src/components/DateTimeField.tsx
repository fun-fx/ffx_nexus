import { Icon } from "./icons";

type DateTimeFieldProps = {
  label: string;
  value: string;
  onChange: (value: string) => void;
  "data-testid"?: string;
  "aria-label"?: string;
};

export function DateTimeField({
  label,
  value,
  onChange,
  "data-testid": testId,
  "aria-label": ariaLabel,
}: DateTimeFieldProps) {
  return (
    <label className="dt-input">
      <span className="dt-input__label">{label}</span>
      <span className={`dt-input__control${value ? "" : " is-empty"}`}>
        <Icon.calendar size={14} className="dt-input__icon" />
        <input
          type="datetime-local"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          aria-label={ariaLabel ?? label}
          data-testid={testId}
        />
      </span>
    </label>
  );
}
