import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { createUser, deleteUser, fetchMe, fetchUsers, type User } from "../api";
import { DataTable, type Column } from "../components/DataTable";
import { Drawer } from "../components/Drawer";
import { StatusPill } from "../components/StatusPill";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";

async function fetchBundle() {
  const [me, users] = await Promise.all([fetchMe(), fetchUsers().catch(() => [])]);
  return { me, users };
}

export function Users() {
  const qc = useQuery({ queryKey: ["users"], queryFn: fetchBundle });
  const [createOpen, setCreateOpen] = useState(false);
  const [error, setError] = useState("");

  const me = qc.data?.me ?? null;
  const users = qc.data?.users ?? [];

  const isAdmin = me?.role === "admin";

  const createMut = useMutation({
    mutationFn: createUser,
    onSuccess: () => {
      setCreateOpen(false);
      qc.refetch();
    },
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
            onClick={() => setCreateOpen(true)}
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

      <div className="panel">
        <DataTable
          rows={users}
          columns={columns}
          rowKey={(u) => u.id}
          emptyMessage="No members yet."
          initialSort={{ id: "email", dir: "asc" }}
        />
      </div>

      <Drawer
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        title="Invite user"
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
              form="invite-user-form"
              disabled={createMut.isPending}
            >
              {createMut.isPending ? "Inviting…" : "Invite"}
            </button>
          </>
        }
      >
        <InviteForm onSubmit={(input) => createMut.mutate(input)} />
      </Drawer>
    </div>
  );
}

function InviteForm({ onSubmit }: { onSubmit: (i: { email: string; password: string; role: string }) => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState("member");
  return (
    <form
      id="invite-user-form"
      className="form-stack"
      onSubmit={(e) => {
        e.preventDefault();
        if (!email.trim() || !password.trim()) return;
        onSubmit({ email: email.trim(), password, role });
      }}
    >
      <label className="field-row">
        <span className="field-label">Email</span>
        <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoFocus />
      </label>
      <label className="field-row">
        <span className="field-label">Initial password</span>
        <input
          type="text"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
        />
      </label>
      <label className="field-row">
        <span className="field-label">Role</span>
        <select value={role} onChange={(e) => setRole(e.target.value)}>
          <option value="member">member</option>
          <option value="admin">admin</option>
        </select>
      </label>
      <p className="muted small">
        The user can change their password after first login. Prefer SSO when available.
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
