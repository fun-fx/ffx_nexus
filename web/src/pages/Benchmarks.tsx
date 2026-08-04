import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  cancelBenchmark,
  clearBenchmarkCredential,
  deleteBenchmark,
  dryRunBenchmark,
  fetchBenchmarkCredential,
  fetchBenchmarkLogs,
  fetchBenchmarkModels,
  fetchBenchmarks,
  launchBenchmark,
  refreshBenchmarks,
  saveBenchmarkCredential,
  type BenchmarkRun,
} from "../api";
import { Chip, type ChipTone } from "../components/Chip";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";

const QUERY_KEY = ["benchmarks"];

// A run can take hours, so the list polls slowly and the Refresh button
// forces a provider read for an operator who is actively watching.
const LIST_REFETCH_MS = 30_000;

const STATUS_TONE: Record<BenchmarkRun["status"], ChipTone> = {
  pending: "info",
  running: "info",
  completed: "ok",
  failed: "err",
  cancelled: "neutral",
};

function isSettled(status: BenchmarkRun["status"]): boolean {
  return status === "completed" || status === "failed" || status === "cancelled";
}

// ENVIRONMENT_PRESETS is what the form offers as one-click picks.
// Prime does not publish a /environments list endpoint — see
// internal/benchmark/runner.go's LaunchSpec note about Hub slugs
// being opaque to Nexus — so the catalog here is curated, not
// discovered. Each preset documents where the dataset comes from
// so an operator picking it knows what they are about to score.
//
// Custom slugs (the operator's own namespace, or a vendor they
// have pushed) go through the Add custom field, not the picker.
const ENVIRONMENT_PRESETS: { slug: string; label: string }[] = [
  { slug: "primeintellect/gsm8k", label: "GSM8K — grade-school math reasoning (1,319 Q)" },
  { slug: "primeintellect/mmlu-pro", label: "MMLU-Pro — multi-task language understanding" },
  { slug: "primeintellect/humaneval", label: "HumanEval — Python coding correctness" },
  { slug: "primeintellect/ifeval", label: "IFEval — instruction-following" },
  { slug: "primeintellect/math-500", label: "Math-500 — competition math" },
  { slug: "verifiers/gsm8k", label: "verifiers/gsm8k — alt grader implementation" },
];

function presetFor(slug: string): { slug: string; label: string } | undefined {
  return ENVIRONMENT_PRESETS.find((p) => p.slug === slug);
}

function formatScore(run: BenchmarkRun): string {
  if (run.avg_score === null || run.avg_score === undefined) return "—";
  return run.avg_score.toFixed(3);
}

function formatWhen(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

export function Benchmarks() {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [logsFor, setLogsFor] = useState<BenchmarkRun | null>(null);

  const list = useQuery({
    queryKey: QUERY_KEY,
    queryFn: fetchBenchmarks,
    refetchInterval: LIST_REFETCH_MS,
  });
  const credential = useQuery({
    queryKey: ["benchmark-credential"],
    queryFn: fetchBenchmarkCredential,
  });

  const runs = list.data?.runs ?? [];
  const gatewayAvailable = list.data?.gateway_routing_available ?? false;
  const maxSamples = list.data?.max_total_samples ?? 0;
  const configured = credential.data?.configured ?? false;

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: QUERY_KEY });
  };
  const report = (e: unknown) => setError(e instanceof Error ? e.message : String(e));

  const refreshM = useMutation({
    mutationFn: refreshBenchmarks,
    onSuccess: (n) => {
      setError(null);
      setNotice(n > 0 ? `Updated ${n} run${n === 1 ? "" : "s"}.` : "No runs changed.");
      invalidate();
    },
    onError: report,
  });
  const cancelM = useMutation({
    mutationFn: cancelBenchmark,
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: report,
  });
  const deleteM = useMutation({
    mutationFn: deleteBenchmark,
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: report,
  });

  const columns: Column<BenchmarkRun>[] = useMemo(
    () => [
      {
        id: "run",
        header: "Run",
        width: 220,
        cell: (r) => (
          <div className="bench-cell-stack">
            <span className="bench-run-name">{r.name || r.model}</span>
            <span className="muted bench-run-sub">{r.model}</span>
          </div>
        ),
        sortValue: (r) => r.name || r.model,
      },
      {
        id: "environments",
        header: "Environments",
        width: 200,
        cell: (r) =>
          r.environments.length === 0 ? (
            <span className="muted">—</span>
          ) : (
            <div className="bench-env-list">
              {r.environments.map((e) => (
                <code key={e} className="bench-env">
                  {e}
                </code>
              ))}
            </div>
          ),
      },
      {
        id: "samples",
        header: "Samples",
        width: 110,
        align: "right",
        cell: (r) => (
          <span title={`${r.num_examples} examples × ${r.rollouts} rollouts`}>
            {r.total_samples ?? r.num_examples * r.rollouts}
          </span>
        ),
        sortValue: (r) => r.total_samples ?? r.num_examples * r.rollouts,
      },
      {
        id: "status",
        header: "Status",
        width: 150,
        cell: (r) => (
          <div className="bench-cell-stack">
            <Chip tone={STATUS_TONE[r.status] ?? "neutral"}>{r.status}</Chip>
            {r.external_status && r.external_status.toLowerCase() !== r.status && (
              <span className="muted bench-run-sub">{r.external_status}</span>
            )}
          </div>
        ),
        sortValue: (r) => r.status,
      },
      {
        id: "score",
        header: "Avg score",
        width: 110,
        align: "right",
        cell: (r) => <strong>{formatScore(r)}</strong>,
        sortValue: (r) => r.avg_score ?? -1,
      },
      {
        id: "measures",
        header: "Measures",
        width: 130,
        cell: (r) =>
          r.via_gateway ? (
            <Chip tone="accent">this gateway</Chip>
          ) : (
            <Chip tone="neutral">provider</Chip>
          ),
      },
      {
        id: "started",
        header: "Started",
        width: 170,
        cell: (r) => <span className="muted">{formatWhen(r.started_at ?? r.created_at)}</span>,
        sortValue: (r) => r.started_at ?? r.created_at,
      },
      {
        id: "actions",
        header: "",
        width: "minmax(0, 1fr)",
        disableResize: true,
        cell: (r) => (
          <div className="bench-actions">
            {r.viewer_url && (
              <a
                className="btn-ghost btn-small"
                href={r.viewer_url}
                target="_blank"
                rel="noreferrer"
              >
                Provider
              </a>
            )}
            {r.external_id && (
              <button type="button" className="btn-ghost btn-small" onClick={() => setLogsFor(r)}>
                Logs
              </button>
            )}
            {!isSettled(r.status) && (
              <button
                type="button"
                className="btn-ghost btn-small"
                disabled={cancelM.isPending}
                onClick={() => cancelM.mutate(r.id)}
              >
                Cancel
              </button>
            )}
            <button
              type="button"
              className="btn-ghost btn-small"
              disabled={deleteM.isPending}
              onClick={() => {
                const warn = isSettled(r.status)
                  ? "Delete this run record?"
                  : "This run is still going at the provider. Deleting it will cancel it first. Continue?";
                if (window.confirm(warn)) deleteM.mutate(r.id);
              }}
            >
              Delete
            </button>
          </div>
        ),
      },
    ],
    [cancelM, deleteM],
  );

  return (
    <div className="page benchmarks-page">
      <header className="page-head">
        <div className="eyebrow">
          <span className="dot" aria-hidden="true" />
          Eval
        </div>
        <h1 className="page-title">
          <GradientText>Model benchmarks</GradientText>
        </h1>
        <p className="page-sub">
          A benchmark measures a <strong>model against a dataset</strong>, which is a different
          question from an eval plugin's "how good was this trace". The dataset and its scoring code
          run on PrimeIntellect's infrastructure — Nexus only launches the run and reads the
          aggregate back, so no eval compute lands in this cluster.
        </p>
      </header>

      {error && (
        <div className="panel bench-alert is-err" role="alert">
          <Icon.x size={14} />
          <span>{error}</span>
          <button type="button" className="btn-ghost btn-small" onClick={() => setError(null)}>
            Dismiss
          </button>
        </div>
      )}
      {notice && (
        <div className="panel bench-alert" role="status">
          <span>{notice}</span>
          <button type="button" className="btn-ghost btn-small" onClick={() => setNotice(null)}>
            Dismiss
          </button>
        </div>
      )}

      <CredentialPanel
        configured={configured}
        onSaved={() => {
          setError(null);
          void qc.invalidateQueries({ queryKey: ["benchmark-credential"] });
        }}
        onError={report}
      />

      <LaunchPanel
        credentialConfigured={configured}
        gatewayAvailable={gatewayAvailable}
        maxSamples={maxSamples}
        onLaunched={(run) => {
          setError(null);
          setNotice(`Launched ${run.name || run.model}. It will settle in the list below.`);
          invalidate();
        }}
        onError={report}
      />

      <section className="panel">
        <div className="panel-head">
          <h2>Runs</h2>
          <button
            type="button"
            className="btn-ghost btn-small"
            disabled={refreshM.isPending}
            onClick={() => refreshM.mutate()}
          >
            {refreshM.isPending ? "Refreshing…" : "Refresh from provider"}
          </button>
        </div>
        <DataTable
          rows={runs}
          columns={columns}
          rowKey={(r) => r.id}
          storageKey="benchmarks"
          initialSort={{ id: "started", dir: "desc" }}
          emptyMessage={
            list.isLoading
              ? "Loading…"
              : "No benchmark runs yet. Launch one above once a provider key and a published environment are in place."
          }
        />
      </section>

      <LogsDrawer run={logsFor} onClose={() => setLogsFor(null)} />
    </div>
  );
}

function CredentialPanel({
  configured,
  onSaved,
  onError,
}: {
  configured: boolean;
  onSaved: () => void;
  onError: (e: unknown) => void;
}) {
  const [value, setValue] = useState("");
  const saveM = useMutation({
    mutationFn: saveBenchmarkCredential,
    onSuccess: () => {
      setValue("");
      onSaved();
    },
    onError,
  });
  const clearM = useMutation({
    mutationFn: clearBenchmarkCredential,
    onSuccess: onSaved,
    onError,
  });

  return (
    <section className="panel">
      <div className="panel-head">
        <h2>PrimeIntellect API key</h2>
        <Chip tone={configured ? "ok" : "warn"}>{configured ? "configured" : "not set"}</Chip>
      </div>
      <p className="hint">
        Encrypted with the Nexus master key and stored in the control-plane database, so it survives
        a deploy. The value is never returned by the API or shown here again. Create one under{" "}
        <a href="https://app.primeintellect.ai" target="_blank" rel="noreferrer">
          your Prime account
        </a>
        .
      </p>
      <div className="bench-credential-row">
        <input
          type="password"
          className="bench-input"
          placeholder="pit_…"
          autoComplete="off"
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
        <button
          type="button"
          className="btn-neon btn-small"
          disabled={!value.trim() || saveM.isPending}
          onClick={() => saveM.mutate(value.trim())}
        >
          {saveM.isPending ? "Saving…" : configured ? "Replace" : "Save"}
        </button>
        {configured && (
          <button
            type="button"
            className="btn-ghost btn-small"
            disabled={clearM.isPending}
            onClick={() => {
              if (window.confirm("Remove the stored PrimeIntellect key?")) clearM.mutate();
            }}
          >
            Remove
          </button>
        )}
      </div>
    </section>
  );
}

function LaunchPanel({
  credentialConfigured,
  gatewayAvailable,
  maxSamples,
  onLaunched,
  onError,
}: {
  credentialConfigured: boolean;
  gatewayAvailable: boolean;
  maxSamples: number;
  onLaunched: (run: BenchmarkRun) => void;
  onError: (e: unknown) => void;
}) {
  const [name, setName] = useState("");
  // Each env is one of two shapes:
  //   - a built-in preset slug string ("primeintellect/gsm8k"),
  //   - a custom slug string the operator typed
  // tagged so we can render the description for presets and a plain
  // chip for custom entries. The union is intentionally string-based;
  // the backend takes a flat string list anyway and we want the
  // payload shape to round-trip without translation.
  const [envs, setEnvs] = useState<{ slug: string; custom: boolean }[]>([
    { slug: "primeintellect/gsm8k", custom: false },
  ]);
  const [model, setModel] = useState("");
  const [numExamples, setNumExamples] = useState(5);
  const [rollouts, setRollouts] = useState(1);
  const [viaGateway, setViaGateway] = useState(false);

  // The catalogue needs the provider key, so only ask for it once one is
  // stored; otherwise the field stays free text.
  const models = useQuery({
    queryKey: ["benchmark-models"],
    queryFn: fetchBenchmarkModels,
    enabled: credentialConfigured,
    staleTime: 5 * 60_000,
    retry: false,
  });

  // The wire-shape sent to the backend is a flat list of slugs. We
  // de-duplicate and drop blanks so the same preset cannot be added
  // twice through a preset+preset collision, but the order is
  // preserved so a list with one custom slug after a preset reads
  // naturally on the wire.
  const envList = Array.from(
    new Set(
      envs
        .map((e) => e.slug.trim())
        .filter(Boolean),
    ),
  );
  const totalSamples = Math.max(0, numExamples) * Math.max(0, rollouts);
  const overCap = maxSamples > 0 && totalSamples > maxSamples;
  const canSubmit =
    credentialConfigured && envList.length > 0 && model.trim() !== "" && !overCap;

  const launchM = useMutation({
    mutationFn: launchBenchmark,
    onSuccess: (run) => {
      setName("");
      onLaunched(run);
    },
    onError,
  });

  // Validate is a guarded ATP — runs only POST + PATCH /cancel
  // with a 1-row sandbox, never persists a benchmark row, and
  // surfaces the vendor's reason verbatim. The form uses its
  // result before Launch to tell operators whether the slug is
  // visible to their account.
  const dryRunM = useMutation({
    mutationFn: dryRunBenchmark,
  });

  return (
    <section className="panel">
      <div className="panel-head">
        <h2>New run</h2>
      </div>

      {!credentialConfigured && (
        <p className="hint-tag warn">Paste a PrimeIntellect API key above before launching a run.</p>
      )}

      <div className="bench-form-grid">
        <label className="field-row">
          <span className="field-label">Name (optional)</span>
          <input
            className="bench-input"
            placeholder="nightly gsm8k"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>

        <label className="field-row bench-field-wide">
          <span className="field-label">Environments</span>
          <div className="bench-env-form">
            <div className="bench-env-preset-row">
              <select
                className="bench-input bench-env-preset"
                aria-label="Add a built-in environment"
                defaultValue=""
                onChange={(e) => {
                  const slug = e.target.value;
                  if (!slug) return;
                  setEnvs((prev) =>
                    prev.some((p) => p.slug === slug)
                      ? prev
                      : [...prev, { slug, custom: false }],
                  );
                  // Reset to placeholder so the same preset can be re-added
                  // after a removal without a separate click cycle.
                  e.target.value = "";
                }}
              >
                <option value="">Add a built-in environment…</option>
                {ENVIRONMENT_PRESETS.map((p) => (
                  <option key={p.slug} value={p.slug}>
                    {p.slug} — {p.label}
                  </option>
                ))}
              </select>
              <form
                className="bench-env-custom"
                onSubmit={(e) => {
                  e.preventDefault();
                  const form = e.currentTarget;
                  const input = form.elements.namedItem(
                    "custom-slug",
                  ) as HTMLInputElement | null;
                  const slug = (input?.value ?? "").trim();
                  if (!slug) return;
                  setEnvs((prev) =>
                    prev.some((p) => p.slug === slug)
                      ? prev
                      : [...prev, { slug, custom: true }],
                  );
                  if (input) input.value = "";
                }}
              >
                <input
                  name="custom-slug"
                  className="bench-input bench-env-custom-input"
                  placeholder="your-org/<dataset-slug>"
                  aria-label="Add a custom environment slug"
                />
                <button
                  type="submit"
                  className="btn-neon btn-ghost btn-small"
                  disabled={!credentialConfigured}
                >
                  Add
                </button>
              </form>
            </div>
            <p className="bench-env-help">
              Pick a built-in dataset or paste a slug you published to
              your Prime namespace with <code>prime env push</code>.
              Visibility is the operator&apos;s responsibility — Nexus
              cannot list templates.
            </p>
            <ul
              className="bench-env-chips"
              data-testid="bench-env-chips"
              aria-label="Selected environments"
            >
              {envs.map((e, idx) => (
                <li key={`${e.slug}-${idx}`} className="bench-env-chip-row">
                  <code className="bench-env">{e.slug}</code>
                  <span className="muted bench-env-chip-note">
                    {e.custom ? "custom" : presetFor(e.slug)?.label ?? "preset"}
                  </span>
                  <button
                    type="button"
                    className="chip-remove"
                    aria-label={`Remove ${e.slug}`}
                    onClick={() =>
                      setEnvs((prev) =>
                        prev.filter((_, i) => i !== idx),
                      )
                    }
                  >
                    ×
                  </button>
                </li>
              ))}
            </ul>
          </div>
        </label>

        <label className="field-row">
          <span className="field-label">Model</span>
          {models.data && models.data.length > 0 ? (
            <select
              className="bench-input"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            >
              <option value="">Select a model…</option>
              {models.data.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.id} — ${m.pricing.prompt}/${m.pricing.completion} per Mtok
                </option>
              ))}
            </select>
          ) : (
            <input
              className="bench-input"
              placeholder="openai/gpt-4.1-mini"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
          )}
        </label>

        <label className="field-row">
          <span className="field-label">Examples</span>
          <input
            className="bench-input"
            type="number"
            min={1}
            value={numExamples}
            onChange={(e) => setNumExamples(Number(e.target.value))}
          />
        </label>

        <label className="field-row">
          <span className="field-label">Rollouts per example</span>
          <input
            className="bench-input"
            type="number"
            min={1}
            value={rollouts}
            onChange={(e) => setRollouts(Number(e.target.value))}
          />
        </label>
      </div>

      <label className="bench-check">
        <input
          type="checkbox"
          checked={viaGateway && gatewayAvailable}
          disabled={!gatewayAvailable}
          onChange={(e) => setViaGateway(e.target.checked)}
        />
        <span>
          <strong>Send the provider's inference through this gateway</strong>
          <span className="muted bench-check-sub">
            {gatewayAvailable
              ? "Scores the model as we actually serve it — routing, cache and provider choice included. A virtual key scoped to just this model is minted for the run and revoked when it settles."
              : "Unavailable: NEXUS_PUBLIC_GATEWAY_URL is not set, so the provider has no address to call back."}
          </span>
        </span>
      </label>

      <div className="bench-submit-row">
        <span className={overCap ? "hint-tag warn" : "hint"}>
          {totalSamples} sample{totalSamples === 1 ? "" : "s"} ({numExamples} × {rollouts})
          {overCap ? ` — over the ${maxSamples} cap` : ""}
        </span>
        <button
          type="button"
          className="btn-neon btn-secondary"
          disabled={!credentialConfigured || envList.length === 0 || model.trim() === "" || dryRunM.isPending}
          onClick={() =>
            dryRunM.mutate({
              environments: envList,
              model: model.trim(),
            })
          }
        >
          {dryRunM.isPending ? "Validating…" : "Validate environments"}
        </button>
        <button
          type="button"
          className="btn-neon"
          disabled={!canSubmit || launchM.isPending}
          onClick={() =>
            launchM.mutate({
              name: name.trim(),
              environments: envList,
              model: model.trim(),
              num_examples: numExamples,
              rollouts,
              via_gateway: viaGateway && gatewayAvailable,
            })
          }
        >
          {launchM.isPending ? "Launching…" : "Launch run"}
        </button>
      </div>
      {dryRunM.isSuccess && dryRunM.data.ok && (
        <p className="hint" data-testid="bench-validate-ok">
          Vendor accepted the credential and the environment slug(s). Safe to launch.
        </p>
      )}
      {dryRunM.isSuccess && !dryRunM.data.ok && (
        <p className="hint-tag warn" data-testid="bench-validate-err">
          {dryRunM.data.error}
        </p>
      )}

      <details className="bench-help">
        <summary>What has to be true before a run will start</summary>
        <ul>
          <li>
            <strong>The environment slug must be visible to your Prime account.</strong>
            Built-in presets (<code>primeintellect/gsm8k</code>, <code>primeintellect/mmlu-pro</code>,
            &hellip;) work for some accounts and not for others — Nexus cannot list
            them. Hit <em>Validate environments</em> before launching: a 404 there
            means the slug is not published to your account yet. The fastest fix is{" "}
            <code>prime env push your-org/&lt;name&gt;</code> on your dataset; once
            one slug is published under your namespace, the rest become visible.
          </li>
          <li>
            <strong>Your Prime wallet needs a balance.</strong> A run bills per
            token, and per sandbox compute when inference is pointed at an
            external endpoint such as this gateway. The model list above shows
            each model&apos;s price in dollars per million tokens so you can
            size the run.
          </li>
          <li>
            <strong>Start small.</strong> 5 examples and 1 rollout is enough to
            prove the wiring before spending on a real measurement.
          </li>
        </ul>
      </details>
    </section>
  );
}

function LogsDrawer({ run, onClose }: { run: BenchmarkRun | null; onClose: () => void }) {
  const logs = useQuery({
    queryKey: ["benchmark-logs", run?.id],
    queryFn: () => fetchBenchmarkLogs(run!.id),
    enabled: Boolean(run?.id),
    retry: false,
  });

  return (
    <Drawer open={Boolean(run)} onClose={onClose} title={`Logs — ${run?.name || run?.model || ""}`}>
      {run?.error && <p className="hint-tag warn">{run.error}</p>}
      {logs.isLoading && <p className="muted">Loading…</p>}
      {logs.isError && (
        <p className="hint-tag warn">
          {logs.error instanceof Error ? logs.error.message : "Could not read logs"}
        </p>
      )}
      {logs.data !== undefined && (
        <pre className="bench-logs">{logs.data || "The provider returned no log output."}</pre>
      )}
    </Drawer>
  );
}
