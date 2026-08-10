import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  createMyCredential,
  deleteMyCredential,
  fetchMyCredentials,
  preflightCredential,
  type Credential,
  type PreflightResult,
} from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { StatusPill } from "../components/StatusPill";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";

// detectionLabels mirror the server's detectProviderFromSecret
// rules so the UI can render a "Looks like an X key" hint without
// having to know the JSON contract. Kept here as well as in the
// server to avoid an unnecessary round-trip just to label a paste.
const detectionLabels: Record<string, string> = {
  openai: "OpenAI",
  anthropic: "Anthropic",
  gemini: "Google Gemini",
};

function detectProviderFromSecret(secret: string): string {
  const s = secret.trim();
  if (s.startsWith("sk-ant-")) return "anthropic";
  if (s.startsWith("sk-proj-") || s.startsWith("sk-")) return "openai";
  if (s.startsWith("AIza")) return "gemini";
  return "";
}

export function Credentials() {
  const qc = useQuery({
    queryKey: ["credentials"],
    queryFn: () => fetchMyCredentials().catch(() => []),
  });
  const [open, setOpen] = useState(false);
  const list = qc.data ?? [];

  const createMut = useMutation({
    mutationFn: createMyCredential,
    onSuccess: () => {
      setOpen(false);
      qc.refetch();
    },
  });

  const rmMut = useMutation({
    mutationFn: deleteMyCredential,
    onSuccess: () => qc.refetch(),
  });

  const columns: Column<Credential>[] = [
    {
      id: "name",
      header: "Name",
      cell: (c) => <strong>{c.name}</strong>,
      sortValue: (c) => c.name,
    },
    {
      id: "provider",
      header: "Provider",
      width: "120px",
      cell: (c) => <Chip tone="accent">{c.provider}</Chip>,
      sortValue: (c) => c.provider,
    },
    {
      id: "last4",
      header: "Key last4",
      width: "120px",
      cell: (c) => <span className="mono">…{c.secret_last4}</span>,
    },
    {
      id: "status",
      header: "Status",
      width: "110px",
      cell: (c) =>
        c.enabled ? (
          <StatusPill label="active" tone="ok" />
        ) : (
          <StatusPill label="off" tone="warn" />
        ),
      sortValue: (c) => Number(c.enabled),
    },
    {
      id: "created",
      header: "Added",
      width: "170px",
      cell: (c) => <span className="mono">{new Date(c.created_at).toLocaleDateString()}</span>,
      sortValue: (c) => c.created_at,
    },
    {
      id: "actions",
      header: "",
      width: "100px",
      align: "right",
      cell: (c) => (
        <button
          type="button"
          className="btn-ghost"
          onClick={(e) => {
            e.stopPropagation();
            if (confirm(`Remove credential "${c.name}"?`)) rmMut.mutate(c.id);
          }}
        >
          Remove
        </button>
      ),
    },
  ];

  return (
    <div className="credentials-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Workspace · BYOK
          </div>
          <h1 className="page-title">
            <GradientText as="span">Provider credentials</GradientText>
          </h1>
          <p className="page-sub">
            Encrypt provider secrets with the org master key. Secrets are sent to the
            gateway only when resolving a call — never logged in plaintext.
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">credentials</div>
            <div className="page-stat-value">{list.length}</div>
          </div>
          <button type="button" className="btn-neon" onClick={() => setOpen(true)}>
            <Icon.shield size={14} /> Add credential
          </button>
        </div>
      </header>

      <div className="panel">
        <DataTable
          rows={list}
          columns={columns}
          rowKey={(c) => c.id}
          emptyMessage="No provider credentials yet. Add one to unblock BYOK-strict models."
          initialSort={{ id: "name", dir: "asc" }}
        />
      </div>

      <Drawer open={open} onClose={() => setOpen(false)} title="Add provider credential">
        <AddCredentialForm
          onSubmit={(input) => createMut.mutate(input)}
          submitting={createMut.isPending}
          error={createMut.error ? String((createMut.error as Error).message) : ""}
          onCancel={() => setOpen(false)}
        />
      </Drawer>
    </div>
  );
}

// AddCredentialForm owns the full pre-flight state machine (probe
// status, in-flight, error) and exposes canSave as a prop on the
// footer slot below the form. Keeping the state co-located here
// avoids lifting every probe tick up to the parent Credentials
// component — the parent only learns the outcome via the
// `onSubmit` callback.
function AddCredentialForm({
  onSubmit,
  submitting,
  error,
  onCancel,
}: {
  onSubmit: (input: Parameters<typeof createMyCredential>[0]) => void;
  submitting: boolean;
  error: string;
  onCancel: () => void;
}) {
  const [provider, setProvider] = useState("openai");
  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [probe, setProbe] = useState<PreflightResult | null>(null);
  const [probeInFlight, setProbeInFlight] = useState(false);
  const [probeError, setProbeError] = useState<string | null>(null);
  // `probedKey` remembers which (provider, secret slice) we have
  // already probed so a re-render that does not change the inputs
  // does not invalidate an old result.
  const [probedKey, setProbedKey] = useState<string>("");

  const detected = useMemo(
    () => (secret.trim() ? detectProviderFromSecret(secret) : ""),
    [secret],
  );
  // Hide the "switch provider" hint while the operator's chosen
  // dropdown already matches the detected shape — those are the
  // boring case and the hint would only add noise.
  const detectionMismatch = detected && detected !== provider;
  // `canSave` is the contract the parent uses to enable Save.
  // A submit is allowed when a fresh, successful probe exists.
  const keyNow = `${provider}|${secret.trim()}|${baseURL.trim()}`;
  const canSave = !!(probe && probe.ok && probedKey === keyNow);

  // Reset the probe whenever the inputs change so the operator
  // cannot commit against a stale result. Backstops the canSave
  // memo above.
  useEffect(() => {
    setProbedKey("");
    setProbe(null);
    setProbeError(null);
  }, [provider, secret, baseURL]);

  const runProbe = () => {
    if (!secret.trim()) return;
    if (probeInFlight) return;
    setProbeInFlight(true);
    setProbeError(null);
    preflightCredential({
      provider,
      secret: secret.trim(),
      base_url: baseURL.trim() || undefined,
    })
      .then((res) => {
        setProbe(res);
        setProbedKey(`${provider}|${secret.trim()}|${baseURL.trim()}`);
      })
      .catch((e: Error) => {
        setProbe(null);
        setProbedKey("");
        setProbeError(e.message || "pre-flight failed");
      })
      .finally(() => setProbeInFlight(false));
  };

  const providerLabel = detectionLabels[detected] ?? detected;

  return (
    <>
      <form
        id="add-cred-form"
        className="form-stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (!secret.trim()) return;
          if (!canSave) return;
          onSubmit({
            provider,
            name: name.trim() || provider,
            secret,
            base_url: baseURL.trim() || undefined,
          });
        }}
      >
        <label className="field-row">
          <span className="field-label">Provider</span>
          <select value={provider} onChange={(e) => setProvider(e.target.value)}>
            <option value="openai">openai</option>
            <option value="anthropic">anthropic</option>
            <option value="gemini">gemini</option>
            <option value="mistral">mistral</option>
            <option value="ollama">ollama (custom base URL)</option>
          </select>
        </label>
        <label className="field-row">
          <span className="field-label">Display name</span>
          <input
            type="text"
            placeholder={provider + "/default"}
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
          />
        </label>
        {detectionMismatch && (
          <div className="detection-hint" role="status">
            <Icon.sparkles size={14} />
            <span>
              Looks like a {providerLabel} key —{" "}
              <button
                type="button"
                className="link-button"
                onClick={() => setProvider(detected)}
              >
                switch provider to {detected}
              </button>
              ?
            </span>
          </div>
        )}
        {provider === "ollama" && (
          <label className="field-row">
            <span className="field-label">Base URL</span>
            <input
              type="text"
              placeholder="http://localhost:11434"
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
              autoComplete="off"
            />
          </label>
        )}
        <label className="field-row">
          <span className="field-label">Secret</span>
          <div className="secret-row">
            <input
              type="password"
              placeholder="sk-… / claude-… / AIza… / Bearer …"
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
              autoComplete="off"
              required
            />
            <button
              type="button"
              className="btn-ghost"
              onClick={runProbe}
              disabled={!secret.trim() || probeInFlight}
            >
              {probeInFlight ? (
                <>
                  <Icon.sparkles size={14} /> Testing…
                </>
              ) : (
                <>Test connection</>
              )}
            </button>
          </div>
        </label>
        {probe && probe.ok && probedKey === keyNow && (
          <div className="probe-result probe-result--ok">
            <StatusPill label="ok" tone="ok" />
            <span>
              Connected to {probe.provider_label}
              {typeof probe.latency_ms === "number" ? ` in ${probe.latency_ms}ms` : ""}
              {probe.status ? ` (HTTP ${probe.status})` : ""}
            </span>
            <Chip tone="ok">{probe.provider_label}</Chip>
          </div>
        )}
        {probe && !probe.ok && probedKey === keyNow && (
          <div className="probe-result probe-result--err">
            <StatusPill label="err" tone="err" />
            <span>
              {probe.provider_label}{" "}
              {probe.status ? `rejected (HTTP ${probe.status})` : "unreachable"}:{" "}
              {probe.message || "pre-flight failed"}
            </span>
          </div>
        )}
        {probeError && (
          <div className="probe-result probe-result--err">
            <StatusPill label="err" tone="err" />
            <span>{probeError}</span>
          </div>
        )}
        <p className="muted small">
          Save is enabled once a free, read-only probe confirms the key works against the
          upstream provider. Encrypted at rest with NEXUS_MASTER_KEY. Used only when the
          gateway resolves a credential for one of your calls.
        </p>
        {error && (
          <div className="auth-error" role="alert">
            {error}
          </div>
        )}
      </form>
      <div className="drawer-footer">
        <button type="button" className="btn-ghost" onClick={onCancel}>
          Cancel
        </button>
        <button
          type="submit"
          form="add-cred-form"
          className="btn-neon"
          disabled={submitting || !canSave}
          title={!canSave ? "Run Test connection to enable Save" : undefined}
        >
          {submitting ? "Saving…" : "Save"}
        </button>
      </div>
    </>
  );
}
