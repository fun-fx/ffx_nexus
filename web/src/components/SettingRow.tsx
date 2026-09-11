import type { ReactNode } from "react";

type SettingRowProps = {
  label: string;
  hint?: string;
  children: ReactNode;
};

export function SettingRow({ label, hint, children }: SettingRowProps) {
  return (
    <div className="setting-row">
      <div className="setting-row__copy">
        <div className="setting-row__label">{label}</div>
        {hint ? <div className="setting-row__hint">{hint}</div> : null}
      </div>
      <div className="setting-row__control">{children}</div>
    </div>
  );
}
