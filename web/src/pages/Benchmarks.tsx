import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  cancelBenchmark,
  clearBenchmarkCredential,
  deleteBenchmark,
  dryRunBenchmark,
  fetchBenchmarkCredential,
  fetchBenchmarkLogs,
  fetchBenchmarkModels,
  fetchEnvPushReports,
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

// splitSlug separates "<owner>/<name>" for the push guide, which needs
// the halves apart: the owner must already exist on the Prime account,
// while only the name may be handed to `prime env init`.
//
// A slug with no slash is treated as a name with an unknown owner
// rather than an error — the guide is what teaches the shape, so it has
// to render something sensible while the operator is still typing.
function splitSlug(slug: string): [owner: string, name: string] {
  const cut = slug.indexOf("/");
  if (cut < 0) return ["your-org", slug];
  return [slug.slice(0, cut), slug.slice(cut + 1)];
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
        storedTeamId={credential.data?.team_id ?? ""}
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
  storedTeamId,
  onSaved,
  onError,
}: {
  configured: boolean;
  storedTeamId: string;
  onSaved: () => void;
  onError: (e: unknown) => void;
}) {
  const [value, setValue] = useState("");
  const [teamId, setTeamId] = useState(storedTeamId);
  useEffect(() => {
    setTeamId(storedTeamId);
  }, [storedTeamId]);
  const teamChanged = teamId.trim() !== storedTeamId.trim();
  const canSave = configured ? value.trim() !== "" || teamChanged : value.trim() !== "";
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
        <h2>PrimeIntellect credentials</h2>
        <Chip tone={configured ? "ok" : "warn"}>{configured ? "configured" : "not set"}</Chip>
      </div>
      <p className="hint">
        Encrypted with the Nexus master key and stored in the control-plane database, so they survive
        a deploy. The API key is never returned or shown here again. Create one under{" "}
        <a href="https://app.primeintellect.ai" target="_blank" rel="noreferrer">
          your Prime account
        </a>
        . Hosted runs bill the <strong>team wallet</strong> when a team ID is set; otherwise Prime
        charges your personal wallet.
      </p>
      <div className="bench-credential-row">
        <input
          type="password"
          className="bench-input"
          placeholder={configured ? "Replace API key (optional)" : "pit_…"}
          autoComplete="off"
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
      </div>
      <div className="bench-credential-row">
        <input
          type="text"
          className="bench-input"
          placeholder="Team ID (optional) — prime teams list"
          autoComplete="off"
          value={teamId}
          onChange={(e) => setTeamId(e.target.value)}
        />
        <button
          type="button"
          className="btn-neon btn-small"
          disabled={!canSave || saveM.isPending}
          onClick={() => {
            const body: { api_key?: string; team_id?: string } = {
              team_id: teamId.trim(),
            };
            if (value.trim()) {
              body.api_key = value.trim();
            }
            saveM.mutate(body);
          }}
        >
          {saveM.isPending ? "Saving…" : configured ? "Save" : "Save"}
        </button>
        {configured && (
          <button
            type="button"
            className="btn-ghost btn-small"
            disabled={clearM.isPending}
            onClick={() => {
              if (window.confirm("Remove the stored PrimeIntellect key and team ID?")) clearM.mutate();
            }}
          >
            Remove
          </button>
        )}
      </div>
      {storedTeamId && (
        <p className="hint">
          Billing team: <code>{storedTeamId}</code> — find IDs with{" "}
          <code>prime teams list</code> or <code>prime whoami</code> after{" "}
          <code>prime switch YOUR-TEAM</code>.
        </p>
      )}
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

  // Is the last dry-run failure a 404 (most common first-visit error)?
  // Case-insensitive because the reason is the vendor's prose, and
  // "Not Found" without the number is a plausible phrasing.
  const dry404 =
    dryRunM.isSuccess && !dryRunM.data.ok && /404|not found/i.test(dryRunM.data.error ?? "");

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
          Vendor accepted the credential and the environment slug(s).
          {dryRunM.data.warning ? (
            <>
              {" "}
              <span className="hint-tag warn" data-testid="bench-validate-warn">
                {dryRunM.data.warning}
              </span>
            </>
          ) : (
            " Safe to launch."
          )}
        </p>
      )}
      {dryRunM.isSuccess && !dryRunM.data.ok && (
        <p className="hint-tag warn" data-testid="bench-validate-err">
          {dryRunM.data.error}
        </p>
      )}
      {dry404 && <EnvPushGuide slugs={envList} />}

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

function EnvPushGuide({ slugs }: { slugs: string[] }) {
  // The first slug is what the operator is most obviously trying to
  // make visible. We use it as the target for the example commands;
  // the rest of the list is shown as chips so the operator can swap
  // any of them in. Falling back to "your-org/gsm8k" keeps the
  // snippet copy-pasteable when the operator removed all chips.
  const target = slugs[0] || "your-org/gsm8k";
  // Prime slugs are <owner>/<name>. The two halves are used in
  // different places: the owner has to already exist on the account,
  // and only the name may appear in the CLI arguments.
  const [owner, name] = splitSlug(target);
  const whichCmd = "which prime  &&  prime --version";
  const whoamiCmd = "prime whoami  &&  prime teams list";
  // `prime env init` maps "-" to "_" but leaves "/" alone, then writes
  // to <path>/<id>/<id>.py without creating the intermediate directory.
  // Passing "<owner>/<name>" therefore crashes on a missing folder, so
  // the name goes in alone and the owner is supplied at push time.
  const initCmd = `mkdir -p ~/prime-envs && cd ~/prime-envs && prime env init ${name} -p .`;
  // No slug argument here, deliberately. With one, the CLI resolves the
  // directory as <path>/<last-segment> — from inside the env folder
  // that means ./<name>/<name>, and the push dies on a missing
  // pyproject.toml. Bare `prime env push` uses the current directory.
  const pushCmd = [
    `cd ~/prime-envs/${name}`,
    `# Personal account:`,
    `prime env push --visibility PRIVATE`,
    `# Team account (replace with your team slug, e.g. ffx-team):`,
    `prime env push --team YOUR-TEAM-SLUG --visibility PRIVATE`,
  ].join("\n");
  // Chained onto the push with && so it only fires on success, and
  // reports failure separately via ||. The operator pastes their own
  // console session token; we cannot template one in because the page
  // authenticates with a cookie the shell does not have.
  const reportCmd = [
    `curl -fsS -X POST ${window.location.origin}/api/eval/benchmarks/push-report \\`,
    `  -H 'Authorization: Bearer <your console token>' \\`,
    `  -H 'Content-Type: application/json' \\`,
    `  -d '{"slug":"${target}","ok":true}'`,
  ].join("\n");

  // The reports list is small and only interesting right after a push,
  // so it is fetched with the guide rather than kept in the page-level
  // query set.
  const reports = useQuery({
    queryKey: ["bench-env-push-reports"],
    queryFn: fetchEnvPushReports,
  });
  const reported = reports.data?.find((r) => r.slug === target);

  return (
    <div className="bench-cli-guide" data-testid="bench-cli-guide">
      <div className="bench-cli-guide-head">
        <h3>Publish the environment yourself</h3>
        <p className="muted">
          Prime does not let Nexus (or any client) push environments on
          the operator&apos;s behalf — that channel is local-CLI only.
          Run these steps on your own workstation, with the same key
          exported as <code>PRIME_API_KEY</code>. Nothing here runs on
          the server hosting this console.
        </p>
        <p className="bench-cli-prereq">
          <strong>Do this first:</strong> the owner half of the slug (
          <code>{owner}</code>) must already be a username or team slug
          on your Prime account, and a fresh account has neither. Set a
          team slug from the team profile on the{" "}
          <a
            href="https://app.primeintellect.ai/dashboard"
            target="_blank"
            rel="noreferrer"
          >
            Prime dashboard
          </a>{" "}
          — a personal username works too, but Prime lets you choose it
          only once and publishes it. Until one exists the push fails at
          the very last step with <em>missing a teamname</em> or{" "}
          <em>missing a username</em>, after the upload appears to
          succeed.
        </p>
      </div>

      {reported && (
        <p
          className={reported.ok ? "hint" : "hint-tag warn"}
          data-testid="bench-push-reported"
        >
          {reported.ok
            ? `A push of ${reported.slug} was reported at ${formatWhen(reported.received_at)}.`
            : `A push of ${reported.slug} was reported as failed at ${formatWhen(reported.received_at)}.`}{" "}
          Reported by the CLI, not verified here — press{" "}
          <em>Validate environments</em> to ask the vendor.
        </p>
      )}

      <ol className="bench-cli-steps">
        <li>
          <span className="bench-cli-step-num">1</span>
          <div className="bench-cli-step-body">
            <strong>Confirm the CLI is installed:</strong>
            <CodeBlock text={whichCmd} />
          </div>
        </li>
        <li>
          <span className="bench-cli-step-num">2</span>
          <div className="bench-cli-step-body">
            <strong>
              Check the owner exists — this is the usual blocker:
            </strong>
            <CodeBlock text={whoamiCmd} />
            <p className="muted">
              You want a real value for <em>Username</em>, or a team
              whose <em>Slug</em> column is filled in. <code>Not set</code>{" "}
              and <code>null</code> both mean the push will fail; go back
              to the dashboard link above and set one.
            </p>
          </div>
        </li>
        <li>
          <span className="bench-cli-step-num">3</span>
          <div className="bench-cli-step-body">
            <strong>Scaffold the environment folder:</strong>
            <CodeBlock text={initCmd} />
            <p className="muted">
              Pass the bare name <code>{name}</code>, not the full slug —{" "}
              <code>prime env init {owner}/{name}</code> crashes with a{" "}
              <code>FileNotFoundError</code> because the CLI builds a
              nested path it never creates. You get{" "}
              <code>~/prime-envs/{name}/</code> holding{" "}
              <code>pyproject.toml</code>, <code>README.md</code> and a{" "}
              <code>{name}.py</code> stub.
            </p>
          </div>
        </li>
        <li>
          <span className="bench-cli-step-num">4</span>
          <div className="bench-cli-step-body">
            <strong>
              Replace the stub — this file <em>is</em> the environment:
            </strong>
            <CodeBlock
              label={`${name}.py`}
              text={SAMPLE_ENV_MODULE}
              downloadName={`${name}.py`}
              language="python"
            />
            <p className="muted">
              Prime builds a wheel from this folder and calls{" "}
              <code>load_environment()</code> in the sandbox, so the
              dataset and the grading live here rather than in separate
              files. This version pulls GSM8K straight from{" "}
              <code>verifiers</code>, so there is nothing else to stage;
              swap in your own <code>dataset</code> and reward function
              when you want to measure something real.
            </p>
          </div>
        </li>
        <li>
          <span className="bench-cli-step-num">5</span>
          <div className="bench-cli-step-body">
            <strong>Publish it:</strong>
            <CodeBlock text={pushCmd} />
            <p className="muted">
              Run it from inside the folder and pass no slug — with one,
              the CLI looks for the environment in{" "}
              <code>./{name}/{name}</code> and reports{" "}
              <code>pyproject.toml not found</code>. Drop{" "}
              <code>--team</code> to publish under your personal
              username, and <code>--visibility PRIVATE</code> to keep it
              off the public hub. Afterwards come back and re-run{" "}
              <em>Validate environments</em>.
            </p>
          </div>
        </li>
        <li>
          <span className="bench-cli-step-num">6</span>
          <div className="bench-cli-step-body">
            <strong>Optional — tell this page it happened:</strong>
            <CodeBlock text={reportCmd} label="report back" />
            <p className="muted">
              Nexus cannot see your terminal, so without this the console
              has no idea a push ever ran. Posting the result puts a
              timestamp on this panel for whoever looks next. It records
              only the slug and pass/fail — no command output, because
              the CLI can echo your API key into it.
            </p>
          </div>
        </li>
      </ol>

      <div className="bench-cli-rotate">
        <strong>If you would rather measure a different model:</strong>
        pick any of your env chips above and re-run this guide with that
        slug as the target.
      </div>
    </div>
  );
}

// SAMPLE_ENV_MODULE is the body the operator drops over the stub that
// `prime env init` scaffolds. It is the whole environment: Prime builds
// a wheel from the folder and calls load_environment() inside the
// sandbox, so there is no separate dataset file and no manifest.
//
// Two details are load-bearing and easy to get wrong:
//
//   - extract_boxed_answer needs strict=True. Without it the helper
//     echoes the entire response when there is no \boxed{}, so the
//     last-number fallback never runs and every unboxed answer scores
//     zero.
//   - the reward function must accept **kwargs. Rubric inspects the
//     signature and only passes the arguments it declares, so a
//     narrower signature silently receives less than it expects.
//
// The dataset comes from verifiers' own load_example_dataset, whose
// default name is literally "gsm8k" — nothing to stage on disk.
const SAMPLE_ENV_MODULE = [
  '"""GSM8K — grade-school math word problems, scored on the final number."""',
  "",
  "import re",
  "",
  "import verifiers as vf",
  "",
  "SYSTEM_PROMPT = (",
  '    "Solve the math problem. Reason step by step, then give the final "',
  '    "numeric answer inside \\\\boxed{}."',
  ")",
  "",
  "",
  "def correct_answer(completion, answer, **kwargs) -> float:",
  '    """1.0 when the model\'s final number matches the reference."""',
  "    text = completion if isinstance(completion, str) else str(completion)",
  "",
  "    # strict=True or the helper returns the whole response when there",
  "    # is no \\boxed{}, starving the fallback below.",
  '    candidate = vf.extract_boxed_answer(text, strict=True) or ""',
  "    if not candidate:",
  "        # Take the last number mentioned — where a chain-of-thought",
  "        # answer almost always lands.",
  '        numbers = re.findall(r"-?\\d[\\d,]*\\.?\\d*", text)',
  '        candidate = numbers[-1] if numbers else ""',
  "",
  "    try:",
  '        return float(float(candidate.replace(",", "")) == float(str(answer).replace(",", "")))',
  "    except ValueError:",
  "        return 0.0",
  "",
  "",
  'def load_environment(num_examples: int = 100, split: str = "test", **kwargs) -> vf.Environment:',
  '    """Load this environment."""',
  '    dataset = vf.load_example_dataset("gsm8k", split=split, n=num_examples)',
  "    rubric = vf.Rubric(funcs=[correct_answer], weights=[1.0])",
  "    return vf.SingleTurnEnv(",
  "        dataset=dataset,",
  "        rubric=rubric,",
  "        system_prompt=SYSTEM_PROMPT,",
  "        **kwargs,",
  "    )",
].join("\n");

function CodeBlock({
  text,
  label,
  downloadName,
  language,
}: {
  text: string;
  label?: string;
  downloadName?: string;
  language?: string;
}) {
  // Per-instance copy state so each block flips its own label
  // "Copy" → "Copied" without racing neighbours.
  const [copied, setCopied] = useState(false);
  const onCopy = async () => {
    try {
      await navigator.clipboard?.writeText(text);
    } catch {
      // Clipboard may fail in non-secure contexts (e.g. http://);
      // not fatal because the operator can still copy by selecting
      // the code block by hand.
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 1600);
  };
  const onDownload = () => {
    // Build a transient object URL for the snippet so the browser
    // gets a real "Save as" prompt with the right filename. Revoked
    // immediately after the click so we don't leak memory.
    const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = downloadName || label || "snippet.txt";
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  };
  return (
    <div
      className={language === "python" ? "bench-cli-code is-py" : "bench-cli-code"}
    >
      <div className="bench-cli-code-head">
        {label && <span className="bench-cli-code-label">{label}</span>}
        <div className="bench-cli-code-actions">
          {downloadName && (
            <button
              type="button"
              className="btn-ghost btn-small"
              data-testid="bench-cli-download"
              onClick={onDownload}
            >
              Download
            </button>
          )}
          <button
            type="button"
            className="btn-ghost btn-small"
            data-testid="bench-cli-copy"
            onClick={onCopy}
          >
            <Icon.copy size={14} /> {copied ? "Copied" : "Copy"}
          </button>
        </div>
      </div>
      <pre><code>{text}</code></pre>
    </div>
  );
}
