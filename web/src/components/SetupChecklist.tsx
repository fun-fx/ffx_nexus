import { useState } from "react";
import { Link } from "react-router-dom";
import { buildSetupSteps, type SetupStep } from "../lib/setup";
import type { AuthConfig, Credential, VirtualKey } from "../api";

const DISMISS_KEY = "nexus:setup-checklist:dismissed";

function readDismissed(): boolean {
  try {
    return window.localStorage.getItem(DISMISS_KEY) === "1";
  } catch {
    return false;
  }
}

export function SetupChecklist({
  cfg,
  credentials,
  keys,
}: {
  cfg?: AuthConfig | null;
  credentials: Credential[];
  keys: VirtualKey[];
}) {
  const [dismissed, setDismissed] = useState(readDismissed);
  const steps = buildSetupSteps({ cfg, credentials, keys });
  const doneCount = steps.filter((s) => s.done).length;
  const remaining = steps.length - doneCount;

  if (remaining === 0 || dismissed) return null;

  function hide() {
    try {
      window.localStorage.setItem(DISMISS_KEY, "1");
    } catch {
      /* private mode */
    }
    setDismissed(true);
  }

  const security = steps.filter((s) => s.section === "security");
  const provider = steps.filter((s) => s.section === "provider");

  return (
    <section className="setup-card" aria-label="Setup checklist" data-testid="setup-checklist">
      <header className="setup-card-head">
        <div>
          <h2>Setup checklist</h2>
          <p className="muted">
            {doneCount} of {steps.length} steps complete.
          </p>
        </div>
        <button type="button" className="btn-ghost" onClick={hide}>
          Remind me later
        </button>
      </header>
      <StepGroup title="Security" steps={security} />
      <StepGroup title="Provider setup" steps={provider} />
    </section>
  );
}

function StepGroup({ title, steps }: { title: string; steps: SetupStep[] }) {
  return (
    <div className="setup-group">
      <h3 className="setup-group-title">{title}</h3>
      <ul className="setup-list">
        {steps.map((s) => (
          <li key={s.id} className={s.done ? "is-done" : ""}>
            <span className="setup-check" aria-hidden="true">
              {s.done ? "✓" : ""}
            </span>
            <div>
              {s.to && !s.done ? (
                <Link to={s.to} className="setup-title">
                  {s.title}
                </Link>
              ) : (
                <span className="setup-title">{s.title}</span>
              )}
              <p className="muted">{s.detail}</p>
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}
