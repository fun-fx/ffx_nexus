import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  createMyCredential,
  deleteMyCredential,
  fetchMyCredentials,
  type Credential,
} from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { StatusPill } from "../components/StatusPill";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";

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

      <Drawer
        open={open}
        onClose={() => setOpen(false)}
        title="Add provider credential"
        footer={
          <>
            <button type="button" className="btn-ghost" onClick={() => setOpen(false)}>
              Cancel
            </button>
            <button
              type="button"
              className="btn-neon"
              form="add-cred-form"
              disabled={createMut.isPending}
            >
              {createMut.isPending ? "Saving…" : "Save"}
            </button>
          </>
        }
      >
        <AddCredentialForm
          onSubmit={(input) => createMut.mutate(input)}
          error={createMut.error ? String((createMut.error as Error).message) : ""}
        />
      </Drawer>
    </div>
  );
}

function AddCredentialForm({
  onSubmit,
  error,
}: {
  onSubmit: (input: Parameters<typeof createMyCredential>[0]) => void;
  error: string;
}) {
  const [provider, setProvider] = useState("openai");
  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");

  return (
    <form
      id="add-cred-form"
      className="form-stack"
      onSubmit={(e) => {
        e.preventDefault();
        if (!secret.trim()) return;
        onSubmit({
          provider,
          name: name.trim() || provider,
          secret,
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
      <label className="field-row">
        <span className="field-label">Secret</span>
        <input
          type="password"
          placeholder="sk-… / claude-… / AIza… / Bearer …"
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
          autoComplete="off"
          required
        />
      </label>
      <p className="muted small">
        Encrypted at rest with NEXUS_MASTER_KEY. Used only when the gateway resolves a
        credential for one of your calls.
      </p>
      {error && (
        <div className="auth-error" role="alert">
          {error}
        </div>
      )}
    </form>
  );
}
