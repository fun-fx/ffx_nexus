import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { Icon } from "../components/icons";
import {
  fetchEvalPlugins,
  fetchMe,
  fetchUIObservability,
  type EvalPluginRecord,
  type UIObservability,
  type User,
} from "../api";
import { parseYamlToForm, PLUGIN_PRESETS } from "../lib/pluginManifest";

type ConnectorId =
  | "otel"
  | "prometheus"
  | "grafana"
  | "metabase"
  | "langfuse"
  | "langsmith"
  | "datadog"
  | "braintrust"
  | "arize"
  | "arize_phoenix"
  | "otel_collector"
  | "confident_ai";

type GatewayId = "otel" | "prometheus" | "grafana" | "metabase";
type EvalKind = Exclude<ConnectorId, GatewayId>;

const GATEWAY: { id: GatewayId; label: string; detail: string }[] = [
  {
    id: "otel",
    label: "OpenTelemetry",
    detail: "Push traces to any OTLP/HTTP collector.",
  },
  {
    id: "prometheus",
    label: "Prometheus",
    detail: "Pull scrape of /metrics on a dedicated listen address.",
  },
  {
    id: "grafana",
    label: "Grafana",
    detail: "Deep-link to the bundled Nexus dashboards.",
  },
  {
    id: "metabase",
    label: "Metabase",
    detail: "Boot-time BI bootstrap against ClickHouse + Postgres.",
  },
];

const EVAL_VENDORS: { id: EvalKind; label: string; detail: string }[] = [
  { id: "langfuse", label: "Langfuse", detail: "Eval plugin — scores come back over webhook or poll." },
  { id: "langsmith", label: "LangSmith", detail: "Eval plugin — traces out, scores back in." },
  { id: "datadog", label: "Datadog", detail: "Eval plugin — LLM observability scores, not APM." },
  { id: "braintrust", label: "Braintrust", detail: "Eval plugin." },
  { id: "arize", label: "Arize", detail: "Eval plugin." },
  { id: "arize_phoenix", label: "Arize Phoenix", detail: "Eval plugin over OTLP." },
  { id: "otel_collector", label: "OTLP collector", detail: "Eval plugin — generic collector, not the gateway sink." },
  { id: "confident_ai", label: "Confident AI", detail: "Eval plugin." },
];

function pluginKind(spec: string): string {
  try {
    return parseYamlToForm(spec).service.kind;
  } catch {
    return "";
  }
}

function scrapeURL(listen: string, local: boolean): string {
  if (!local) return "";
  const port = listen.replace(/^.*:/, "");
  if (!port) return "";
  if (typeof window === "undefined") return `http://127.0.0.1:${port}/metrics`;
  return `${window.location.protocol}//${window.location.hostname}:${port}/metrics`;
}

function CopyField({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false);
  if (!value) return null;
  return (
    <div className="obs-copy">
      <div className="obs-copy-label">{label}</div>
      <div className="obs-copy-row">
        <code>{value}</code>
        <button
          type="button"
          className="btn-ghost"
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(value);
              setCopied(true);
              window.setTimeout(() => setCopied(false), 1500);
            } catch {
              /* tests / http */
            }
          }}
        >
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
    </div>
  );
}

function StatusToggle({ on, label }: { on: boolean; label: string }) {
  return (
    <div className="obs-status">
      <span className={"obs-switch" + (on ? " is-on" : "")} aria-hidden="true" />
      <span>
        {label}: {on ? "on" : "off"}
      </span>
      <Chip tone={on ? "ok" : "info"}>{on ? "Enabled" : "Not configured"}</Chip>
    </div>
  );
}

export function Observability() {
  const [selected, setSelected] = useState<ConnectorId>("otel");
  const obs = useQuery({
    queryKey: ["ui-observability"],
    queryFn: fetchUIObservability,
  });
  const me = useQuery({ queryKey: ["me"], queryFn: fetchMe });
  const plugins = useQuery({
    queryKey: ["eval-plugins"],
    queryFn: fetchEvalPlugins,
    enabled: me.data?.role === "admin",
  });

  const cfg: UIObservability = obs.data ?? {};
  const user: User | null = me.data ?? null;
  const isAdmin = user?.role === "admin";
  const rows = plugins.data ?? [];

  const byKind = useMemo(() => {
    const map = new Map<string, EvalPluginRecord[]>();
    for (const p of rows) {
      const k = pluginKind(p.spec_yaml);
      if (!k) continue;
      const list = map.get(k) ?? [];
      list.push(p);
      map.set(k, list);
    }
    return map;
  }, [rows]);

  function gatewayOn(id: GatewayId): boolean {
    if (id === "otel") return Boolean(cfg.otlp?.enabled);
    if (id === "prometheus") return Boolean(cfg.prometheus?.enabled);
    if (id === "grafana") return Boolean(cfg.grafana?.base);
    if (id === "metabase") return Boolean(cfg.metabase?.configured);
    return false;
  }

  function evalOn(id: EvalKind): boolean {
    const list = byKind.get(id) ?? [];
    return list.some((p) => p.enabled);
  }

  return (
    <div className="observability-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Workspace · observability
          </div>
          <h1 className="page-title">
            <GradientText as="span">Observability connectors</GradientText>
          </h1>
          <p className="page-sub">
            Gateway sinks are boot config (env / Helm). Eval vendors are
            plugins you can enable on the Eval page. Ports stay split:
            console :8081, gateway :8080, Prometheus scrape on its own listen
            address.
          </p>
        </div>
      </header>

      <div className="obs-layout">
        <nav className="obs-providers" aria-label="Connectors">
          <div className="obs-group-label">Gateway sinks</div>
          {GATEWAY.map((c) => (
            <button
              key={c.id}
              type="button"
              className={
                "obs-provider" + (selected === c.id ? " is-active" : "")
              }
              onClick={() => setSelected(c.id)}
            >
              <span>{c.label}</span>
              <span className={"obs-dot" + (gatewayOn(c.id) ? " is-on" : "")} />
            </button>
          ))}
          <div className="obs-group-label">Eval vendors</div>
          {EVAL_VENDORS.map((c) => (
            <button
              key={c.id}
              type="button"
              className={
                "obs-provider" + (selected === c.id ? " is-active" : "")
              }
              onClick={() => setSelected(c.id)}
            >
              <span>{c.label}</span>
              <span className={"obs-dot" + (evalOn(c.id) ? " is-on" : "")} />
            </button>
          ))}
        </nav>

        <section className="panel obs-detail" aria-label="Connector settings">
          {GATEWAY.some((c) => c.id === selected) ? (
            <GatewayPanel id={selected as GatewayId} cfg={cfg} />
          ) : (
            <EvalPanel
              id={selected as EvalKind}
              plugins={byKind.get(selected) ?? []}
              isAdmin={isAdmin}
            />
          )}
        </section>
      </div>
    </div>
  );
}

function GatewayPanel({ id, cfg }: { id: GatewayId; cfg: UIObservability }) {
  if (id === "otel") {
    const on = Boolean(cfg.otlp?.enabled);
    return (
      <>
        <header className="obs-detail-head">
          <div>
            <h2>OpenTelemetry</h2>
            <p className="muted">
              Gateway traces POST as OTLP/HTTP JSON to{" "}
              <code>NEXUS_OTLP_ENDPOINT</code>. This is not the eval-plugin
              collector — that one lives under Eval vendors.
            </p>
          </div>
          <StatusToggle on={on} label="Export" />
        </header>
        <CopyField value={cfg.otlp?.endpoint ?? ""} label="Collector endpoint" />
        <pre className="obs-snippet">{`# env
NEXUS_OTLP_ENABLED=true
NEXUS_OTLP_ENDPOINT=http://localhost:4318/v1/traces

# Helm
config:
  otlp:
    enabled: true
    endpoint: http://otel-collector:4318/v1/traces`}</pre>
        <p className="muted small">
          Changing this from the console would not survive a restart. Edit env
          or Helm and roll the process.
        </p>
      </>
    );
  }
  if (id === "prometheus") {
    const on = Boolean(cfg.prometheus?.enabled);
    const listen = cfg.prometheus?.listen ?? "";
    const url = on ? scrapeURL(listen, Boolean(cfg.local_mode)) : "";
    return (
      <>
        <header className="obs-detail-head">
          <div>
            <h2>Prometheus</h2>
            <p className="muted">
              Pull-based scrape. Nexus does not push to Prometheus. The
              scrape server is a separate listen from the gateway (:8080) on
              purpose.
            </p>
          </div>
          <StatusToggle on={on} label="Pull scrape" />
        </header>
        <CopyField value={url} label="Scrape URL" />
        <CopyField value={listen} label="Listen address" />
        <pre className="obs-snippet">{`# env
NEXUS_METRICS_ADDR=:9100

# Helm
metrics:
  enabled: true
  port: 9100
  path: /metrics`}</pre>
        <p className="muted small">
          Point Prometheus at that URL. Served only while{" "}
          <code>NEXUS_METRICS_ADDR</code> is set.
        </p>
      </>
    );
  }
  if (id === "grafana") {
    const g = cfg.grafana;
    return (
      <>
        <header className="obs-detail-head">
          <div>
            <h2>Grafana</h2>
            <p className="muted">
              Link-only. Nexus never stores a Grafana credential and never calls
              Grafana on the request path.
            </p>
          </div>
          <StatusToggle on={Boolean(g?.base)} label="Public URL" />
        </header>
        {g ? (
          <div className="obs-links">
            <a className="btn-neon" href={g.overview} target="_blank" rel="noreferrer">
              Overview dashboard
            </a>
            <a className="btn-ghost" href={g.spend} target="_blank" rel="noreferrer">
              Spend
            </a>
            <a className="btn-ghost" href={g.eval} target="_blank" rel="noreferrer">
              Eval quality
            </a>
          </div>
        ) : (
          <pre className="obs-snippet">{`NEXUS_PUBLIC_GRAFANA_URL=http://localhost:3000

# compose
docker compose -f deploy/docker-compose.yml --profile observability up -d`}</pre>
        )}
      </>
    );
  }
  const on = Boolean(cfg.metabase?.configured);
  return (
    <>
      <header className="obs-detail-head">
        <div>
          <h2>Metabase</h2>
          <p className="muted">
            One-shot boot adapter. Registers ClickHouse + Postgres and seeds
            the bundled collections. Not a hot-path sink.
          </p>
        </div>
        <StatusToggle on={on} label="Bootstrap" />
      </header>
      <pre className="obs-snippet">{`NEXUS_METABASE_URL=http://localhost:3001
NEXUS_METABASE_USER=admin@nexus.local
NEXUS_METABASE_PASSWORD=…

# compose
docker compose -f deploy/docker-compose.yml --profile bi up -d`}</pre>
    </>
  );
}

function EvalPanel({
  id,
  plugins,
  isAdmin,
}: {
  id: EvalKind;
  plugins: EvalPluginRecord[];
  isAdmin: boolean;
}) {
  const meta = EVAL_VENDORS.find((v) => v.id === id);
  const preset = PLUGIN_PRESETS[id]?.label ?? id;
  const live = plugins.filter((p) => p.enabled);
  return (
    <>
      <header className="obs-detail-head">
        <div>
          <h2>{meta?.label ?? id}</h2>
          <p className="muted">
            {meta?.detail} This is an <strong>eval plugin</strong> — Nexus
            forwards traces, the vendor scores them, scores come back as{" "}
            <code>gen_ai.evaluation.result</code>. It is not the gateway OTLP
            sink.
          </p>
        </div>
        <StatusToggle on={live.length > 0} label="Plugin" />
      </header>
      {plugins.length > 0 ? (
        <ul className="obs-plugin-list">
          {plugins.map((p) => (
            <li key={p.name}>
              <strong>{p.name}</strong>
              <Chip tone={p.enabled ? "ok" : "info"}>
                {p.enabled ? "enabled" : "disabled"}
              </Chip>
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted">No plugin of this kind is installed yet.</p>
      )}
      {isAdmin ? (
        <p>
          <Link to="/eval?focus=plugins" className="btn-neon">
            <Icon.sparkles size={14} /> Open Eval plugins
          </Link>
          <span className="muted small">
            {" "}
            Preset: {preset}
          </span>
        </p>
      ) : (
        <p className="muted">Ask an admin to add this plugin on the Eval page.</p>
      )}
    </>
  );
}
