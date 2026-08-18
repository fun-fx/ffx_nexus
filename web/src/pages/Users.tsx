import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  createInvite,
  deleteUser,
  fetchMe,
  fetchUsers,
  listInvites,
  revokeInvite,
  type InviteIssued,
  type User,
} from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { StatusPill } from "../components/StatusPill";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";

async function fetchBundle() {
  const [me, users, invites] = await Promise.all([
    fetchMe(),
    fetchUsers().catch(() => []),
    listInvites().catch(() => []),
  ]);
  return { me, users, invites };
}

export function Users() {
  const qc = useQuery({ queryKey: ["users"], queryFn: fetchBundle });
  const [inviteOpen, setInviteOpen] = useState(false);
  const [lastInvite, setLastInvite] = useState<InviteIssued | null>(null);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState(false);

  const me = qc.data?.me ?? null;
  const users = qc.data?.users ?? [];
  const invites = qc.data?.invites ?? [];

  const isAdmin = me?.role === "admin";

  const inviteMut = useMutation({
    mutationFn: createInvite,
    onSuccess: (inv) => {
      setLastInvite(inv);
      setCopied(false);
      qc.refetch();
    },
    onError: (e) => setError(String((e as Error).message)),
  });

  const revokeMut = useMutation({
    mutationFn: revokeInvite,
    onSuccess: () => qc.refetch(),
    onError: (e) => setError(String((e as Error).message)),
  });

  const deleteMut = useMutation({
    mutationFn: deleteUser,
    onSuccess: () => qc.refetch(),
    onError: (e) => setError(String((e as Error).message)),
  });

  if (!isAdmin) {
    return <Forbidden />;
  }

  const inviteColumns: Column<InviteRowForTable>[] = [
    {
      id: "email",
      header: "Email",
      cell: (inv) => <strong>{inv.email}</strong>,
      sortValue: (inv) => inv.email,
    },
    {
      id: "role",
      header: "Role",
      width: "110px",
      cell: (inv) =>
        inv.role === "admin" ? (
          <Chip tone="accent">admin</Chip>
        ) : (
          <Chip tone="neutral">member</Chip>
        ),
      sortValue: (inv) => inv.role,
    },
    {
      id: "status",
      header: "Status",
      width: "140px",
      cell: (inv) => {
        if (inv.revoked_at) return <StatusPill label="revoked" tone="err" />;
        if (inv.accepted_at) return <StatusPill label="accepted" tone="ok" />;
        const exp = Date.parse(inv.expires_at);
        if (!Number.isNaN(exp) && exp < Date.now()) {
          return <StatusPill label="expired" tone="err" />;
        }
        return <StatusPill label="pending" tone="warn" />;
      },
      sortValue: (inv) =>
        inv.revoked_at ?? inv.accepted_at ?? inv.expires_at ?? "",
    },
    {
      id: "expires",
      header: "Expires",
      width: "170px",
      cell: (inv) => (
        <span className="mono">{new Date(inv.expires_at).toLocaleString()}</span>
      ),
      sortValue: (inv) => inv.expires_at,
    },
    {
      id: "actions",
      header: "",
      width: "110px",
      align: "right",
      cell: (inv) =>
        inv.accepted_at || inv.revoked_at ? (
          <span className="muted">closed</span>
        ) : (
          <button
            type="button"
            className="btn-ghost"
            disabled={revokeMut.isPending}
            onClick={() => revokeMut.mutate(inv.id)}
          >
            Revoke
          </button>
        ),
    },
  ];

  const columns: Column<User>[] = [
    {
      id: "email",
      header: "Email",
      cell: (u) => <strong>{u.email}</strong>,
      sortValue: (u) => u.email,
    },
    {
      id: "role",
      header: "Role",
      width: "110px",
      cell: (u) =>
        u.role === "admin" ? (
          <Chip tone="accent">admin</Chip>
        ) : (
          <Chip tone="neutral">member</Chip>
        ),
      sortValue: (u) => u.role,
    },
    {
      id: "limits",
      header: "Enforce limits",
      width: "140px",
      cell: (u) =>
        u.enforce_limits ? (
          <StatusPill label="on" tone="ok" />
        ) : (
          <StatusPill label="off" tone="warn" />
        ),
      sortValue: (u) => Number(u.enforce_limits),
    },
    {
      id: "onboarded",
      header: "Onboarded",
      width: "170px",
      cell: (u) =>
        u.onboarded_at ? (
          <span className="mono">
            {new Date(u.onboarded_at).toLocaleDateString()}
          </span>
        ) : (
          <Chip tone="warn">pending</Chip>
        ),
      sortValue: (u) => u.onboarded_at ?? "",
    },
    {
      id: "created",
      header: "Created",
      width: "170px",
      cell: (u) => (
        <span className="mono">{new Date(u.created_at).toLocaleString()}</span>
      ),
      sortValue: (u) => u.created_at,
    },
    {
      id: "actions",
      header: "",
      width: "110px",
      align: "right",
      cell: (u) =>
        u.id === me?.id ? (
          <span className="muted">self</span>
        ) : (
          <button
            type="button"
            className="btn-ghost"
            onClick={(e) => {
              e.stopPropagation();
              if (confirm(`Remove ${u.email}? Their keys and traces will go with them.`))
                deleteMut.mutate(u.id);
            }}
            disabled={deleteMut.isPending}
          >
            Remove
          </button>
        ),
    },
  ];

  return (
    <div className="users-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Admin · users
          </div>
          <h1 className="page-title">
            <GradientText as="span">Members</GradientText>
          </h1>
          <p className="page-sub">Members of this org and their roles.</p>
        </div>
        <div className="page-stats">
          <div className="page-stat">
            <div className="page-stat-label">total</div>
            <div className="page-stat-value">{users.length}</div>
          </div>
          <button
            type="button"
            className="btn-neon"
            onClick={() => {
              setLastInvite(null);
              setCopied(false);
              setInviteOpen(true);
            }}
          >
            <Icon.users size={14} /> Invite user
          </button>
        </div>
      </header>

      {error && (
        <div className="auth-error" role="alert" style={{ marginBottom: 0 }}>
          {error}
          <button
            type="button"
            className="btn-ghost"
            style={{ marginLeft: 8 }}
            onClick={() => setError("")}
          >
            Dismiss
          </button>
        </div>
      )}

      {lastInvite && (
        <div className="invite-result" data-testid="invite-result">
          <div className="invite-result-row">
            <span className="invite-result-label">Invite URL for {lastInvite.email}</span>
            <button
              type="button"
              className="btn-ghost"
              data-testid="invite-copy"
              onClick={async () => {
                if (!lastInvite.url) return;
                try {
                  await navigator.clipboard.writeText(lastInvite.url);
                  setCopied(true);
                } catch {
                  setCopied(false);
                }
              }}
            >
              {copied ? "Copied" : "Copy"}
            </button>
          </div>
          <code className="invite-result-url mono" data-testid="invite-url">
            {lastInvite.url}
          </code>
          <p className="muted small">
            Send this link to the invitee. It expires{" "}
            {new Date(lastInvite.expires_at).toLocaleString()} and can be
            revoked from the table below.
          </p>
        </div>
      )}

      <div className="panel">
        <h2 className="panel-subtitle">Members</h2>
        <DataTable
          rows={users}
          columns={columns}
          rowKey={(u) => u.id}
          emptyMessage="No members yet."
          initialSort={{ id: "email", dir: "asc" }}
        />
      </div>

      <div className="panel">
        <h2 className="panel-subtitle">Invites</h2>
        <DataTable
          rows={invites as InviteRowForTable[]}
          columns={inviteColumns}
          rowKey={(inv) => inv.id}
          emptyMessage="No invites yet."
          initialSort={{ id: "expires", dir: "asc" }}
        />
      </div>

      <Drawer
        open={inviteOpen}
        onClose={() => setInviteOpen(false)}
        title="Invite user"
        footer={
          <>
            <button
              type="button"
              className="btn-ghost"
              onClick={() => setInviteOpen(false)}
            >
              Cancel
            </button>
            <button
              type="button"
              className="btn-neon"
              form="invite-user-form"
              disabled={inviteMut.isPending}
            >
              {inviteMut.isPending ? "Issuing…" : "Issue invite"}
            </button>
          </>
        }
      >
        <InviteForm onSubmit={(input) => inviteMut.mutate(input)} />
      </Drawer>
    </div>
  );
}

type InviteRowForTable = {
  id: string;
  email: string;
  role: string;
  created_by?: string;
  created_at?: string;
  expires_at: string;
  accepted_at?: string | null;
  revoked_at?: string | null;
  accepted_by?: string | null;
};

function InviteForm({ onSubmit }: { onSubmit: (i: { email: string; role: string }) => void }) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("member");
  return (
    <form
      id="invite-user-form"
      className="form-stack"
      onSubmit={(e) => {
        e.preventDefault();
        if (!email.trim()) return;
        onSubmit({ email: email.trim(), role });
      }}
    >
      <label className="field-row">
        <span className="field-label">Email</span>
        <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoFocus />
      </label>
      <label className="field-row">
        <span className="field-label">Role</span>
        <select value={role} onChange={(e) => setRole(e.target.value)}>
          <option value="member">member</option>
          <option value="admin">admin</option>
        </select>
      </label>
      <p className="muted small">
        Creates a one-time invite link the invitee accepts in their
        browser. The invite expires in seven days and can be revoked
        from the table below.
      </p>
    </form>
  );
}

function Forbidden() {
  return (
    <div className="placeholder-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Admin · members
          </div>
          <h1 className="page-title">
            <GradientText as="span">Forbidden</GradientText>
          </h1>
          <p className="page-sub">Only admin accounts can view this page.</p>
        </div>
      </header>
    </div>
  );
}
