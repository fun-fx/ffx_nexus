import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  cancelBenchmark,
  clearBenchmarkCredential,
  deleteBenchmark,
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
  const [environments, setEnvironments] = useState("");
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

  const envList = environments
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
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
          <span className="field-label">
            Environments — one Hub slug per line, or comma separated
          </span>
          <textarea
            className="bench-input bench-textarea"
            rows={3}
            placeholder={"your-org/gsm8k\nyour-org/alphabet-sort"}
            value={environments}
            onChange={(e) => setEnvironments(e.target.value)}
          />
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

      <details className="bench-help">
        <summary>What has to be true before a run will start</summary>
        <ul>
          <li>
            <strong>The environment must be published to the Prime Environments Hub</strong>, and
            your account needs write access to it. An environment is a dataset plus the Python
            scoring code that grades an answer. There is no public environments API, so Nexus cannot
            offer a picker — paste the slug you own.
          </li>
          <li>
            <strong>Your Prime wallet needs a balance.</strong> A run bills per token, and per
            sandbox compute when inference is pointed at an external endpoint such as this gateway.
          </li>
          <li>
            <strong>Start small.</strong> 5 examples and 1 rollout is enough to prove the wiring
            before spending on a real measurement.
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
