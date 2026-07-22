import { Icon } from "./icons";

interface Props {
  label: string;
  value?: React.ReactNode;
  tone?: "neutral" | "ok" | "warn" | "err";
}

export function StatusPill({ label, value, tone = "neutral" }: Props) {
  const icon =
    tone === "ok" ? <Icon.check size={12} /> :
    tone === "warn" ? <span aria-hidden="true">!</span> :
    tone === "err" ? <Icon.x size={12} /> :
    null;
  return (
    <span className={`status-pill is-${tone}`}>
      {icon && <span className="status-pill-icon" aria-hidden="true">{icon}</span>}
      <span className="status-pill-label">{label}</span>
      {value !== undefined && value !== null && (
        <span className="status-pill-value">{value}</span>
      )}
    </span>
  );
}
