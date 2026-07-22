import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  createMyKey,
  fetchMe,
  fetchMyKeys,
  type VirtualKey,
} from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { StatusPill } from "../components/StatusPill";
import { GradientText } from "../components/GradientText";
import { Chip } from "../components/Chip";
import { Icon } from "../components/icons";

async function fetchBundle() {
  const me = await fetchMe();
  const keys = await fetchMyKeys();
  return { me, keys };
}

export function Keys() {
  const qc = useQuery({ queryKey: ["keys"], queryFn: fetchBundle });
  const [createOpen, setCreateOpen] = useState(false);
  const [created, setCreated] = useState<{ key: VirtualKey; secret: string } | null>(null);

  const user = qc.data?.me ?? null;
  const keys = qc.data?.keys ?? [];

  const createMut = useMutation({
    mutationFn: (name: string) => createMyKey(name),
    onSuccess: (res) => {
      setCreated(res);
      setCreateOpen(false);
      qc.refetch();
    },
  });

  const revokeMut = useMutation({
    mutationFn: async (id: string) => {
      await fetch(`/api/me/keys/${id}`, { method: "DELETE" });
    },
    onSuccess: () => qc.refetch(),
  });

  const columns: Column<VirtualKey>[] = [
    {
      id: "name",
      header: "Name",
      cell: (k) => <strong>{k.name}</strong>,
      sortValue: (k) => k.name,
    },
    {
      id: "prefix",
      header: "Prefix",
      width: "160px",
      cell: (k) => <span className="mono">{k.key_prefix}…</span>,
      sortValue: (k) => k.key_prefix,
    },
    {
      id: "status",
      header: "Status",
      width: "110px",
      cell: (k) =>
        k.revoked ? (
          <StatusPill label="revoked" tone="err" />
        ) : (
          <StatusPill label="active" tone="ok" />
        ),
      sortValue: (k) => Number(k.revoked),
    },
    {
      id: "rpm",
      header: "RPM",
      width: "80px",
      align: "right",
      cell: (k) => <span className="mono">{k.rpm_limit || "∞"}</span>,
      sortValue: (k) => k.rpm_limit,
    },
    {
      id: "budget",
      header: "Monthly budget",
      width: "140px",
      align: "right",
      cell: (k) => (
        <span className="mono">
          {k.monthly_budget_usd ? `$${k.monthly_budget_usd.toFixed(2)}` : "—"}
        </span>
      ),
      sortValue: (k) => k.monthly_budget_usd,
    },
    {
      id: "models",
      header: "Models",
      cell: (k) =>
        !k.allowed_models || k.allowed_models.length === 0 ? (
          <Chip tone="ok">all</Chip>
        ) : (
          <span className="chip-row">
            {(k.allowed_models ?? []).slice(0, 3).map((m) => (
              <Chip key={m} tone="info">
                {m}
              </Chip>
            ))}
            {(k.allowed_models ?? []).length > 3 && (
              <Chip tone="neutral">
                +{(k.allowed_models ?? []).length - 3}
              </Chip>
            )}
          </span>
        ),
    },
    {
      id: "actions",
      header: "",
      width: "100px",
      align: "right",
      cell: (k) =>
        k.revoked ? (
          <span className="muted">—</span>
        ) : (
          <button
            type="button"
            className="btn-ghost"
            onClick={(e) => {
              e.stopPropagation();
              if (confirm(`Revoke key "${k.name}"? This cannot be undone.`))
                revokeMut.mutate(k.id);
            }}
            disabled={revokeMut.isPending}
          >
            Revoke
          </button>
        ),
    },
  ];

  return (
    <div className="keys-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Workspace · virtual keys
          </div>
          <h1 className="page-title">
            <GradientText as="span">Keys</GradientText>
          </h1>
          <p className="page-sub">
            {user
              ? `Personal keys for ${user.email}. Share the secret only after generating it — it is shown once.`
              : "Sign in to manage your personal API keys."}
          </p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">keys</div>
            <div className="page-stat-value">{keys.length}</div>
          </div>
          <button
            type="button"
            className="btn-neon"
            onClick={() => setCreateOpen(true)}
          >
            <Icon.keys size={14} /> New key
          </button>
        </div>
      </header>

      <div className="panel">
        <DataTable
          rows={keys}
          columns={columns}
          rowKey={(k) => k.id}
          emptyMessage="No keys yet — press “New key” to create one."
          initialSort={{ id: "name", dir: "asc" }}
        />
      </div>

      <Drawer
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        title="Create virtual key"
        footer={
          <>
            <button
              type="button"
              className="btn-ghost"
              onClick={() => setCreateOpen(false)}
            >
              Cancel
            </button>
            <button
              type="button"
              className="btn-neon"
              form="create-key-form"
              disabled={createMut.isPending}
            >
              {createMut.isPending ? "Creating…" : "Create"}
            </button>
          </>
        }
      >
        <CreateKeyForm
          onSubmit={(name) => createMut.mutate(name)}
          error={createMut.error ? String((createMut.error as Error).message) : ""}
        />
      </Drawer>

      <Drawer
        open={Boolean(created)}
        onClose={() => setCreated(null)}
        title="Key created"
        footer={
          <button
            type="button"
            className="btn-neon"
            onClick={() => setCreated(null)}
          >
            Done
          </button>
        }
      >
        {created && <NewKeySecret block={created} />}
      </Drawer>
    </div>
  );
}

function CreateKeyForm({
  onSubmit,
  error,
}: {
  onSubmit: (name: string) => void;
  error: string;
}) {
  const [name, setName] = useState("");
  return (
    <form
      id="create-key-form"
      className="form-stack"
      onSubmit={(e) => {
        e.preventDefault();
        if (!name.trim()) return;
        onSubmit(name.trim());
      }}
    >
      <label className="field-row">
        <span className="field-label">Key name</span>
        <input
          type="text"
          placeholder="e.g. cb-playground"
          value={name}
          onChange={(e) => setName(e.target.value)}
          autoFocus
          required
        />
      </label>
      <p className="muted small">
        The plaintext secret appears only once after creation. Store it somewhere safe.
      </p>
      {error && (
        <div className="auth-error" role="alert">
          {error}
        </div>
      )}
    </form>
  );
}

function NewKeySecret({ block }: { block: { key: VirtualKey; secret: string } }) {
  const [copied, setCopied] = useState(false);
  const text = block.secret;
  const curl = `# Test
curl https://api.ffx.ai/v1/chat/completions \\
  -H "Authorization: Bearer ${text}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"auto","messages":[{"role":"user","content":"hello"}]}'`;
  return (
    <div className="new-key-secret">
      <div className="callout">
        This is the only time the plaintext is shown. Copy it now.
      </div>
      <div className="secret-row">
        <code>{text}</code>
        <button
          type="button"
          className="btn-ghost"
          onClick={() => {
            navigator.clipboard?.writeText(text);
            setCopied(true);
            setTimeout(() => setCopied(false), 1600);
          }}
        >
          <Icon.copy size={14} /> {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <label className="field-row">
        <span className="field-label">Smoke-test curl</span>
        <pre className="curl-snippet">{curl}</pre>
      </label>
    </div>
  );
}
