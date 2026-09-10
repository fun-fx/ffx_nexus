import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { buildSetupSteps, type SetupStep } from "../lib/setup";
import type { AuthConfig, Credential, VirtualKey } from "../api";

function dismissKey(userId?: string | null): string {
  return userId
    ? `nexus:setup-checklist:dismissed:${userId}`
    : "nexus:setup-checklist:dismissed";
}

function readDismissed(userId?: string | null): boolean {
  try {
    return window.localStorage.getItem(dismissKey(userId)) === "1";
  } catch {
    return false;
  }
}

export function SetupChecklist({
  cfg,
  credentials,
  keys,
  userId,
}: {
  cfg?: AuthConfig | null;
  credentials: Credential[];
  keys: VirtualKey[];
  userId?: string | null;
}) {
  const [dismissed, setDismissed] = useState(() => readDismissed(userId));

  useEffect(() => {
    setDismissed(readDismissed(userId));
  }, [userId]);

  const steps = buildSetupSteps({ cfg, credentials, keys });
  const doneCount = steps.filter((s) => s.done).length;
  const remaining = steps.length - doneCount;
  const complete = remaining === 0;

  // Bifrost-style: stay visible until the operator dismisses it, even when
  // every step is already green (typical on a long-running prod tenant).
  if (dismissed) return null;

  function hide() {
    try {
      window.localStorage.setItem(dismissKey(userId), "1");
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
            {complete
              ? "All steps complete — you're ready to route traffic."
              : `${doneCount} of ${steps.length} steps complete.`}
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
