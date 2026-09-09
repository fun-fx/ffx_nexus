import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchGatewayConfig, patchGatewayConfig } from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";

export function Alerting() {
  const qc = useQueryClient();
  const cfgQ = useQuery({ queryKey: ["gateway-config"], queryFn: fetchGatewayConfig });
  const alert = cfgQ.data?.alerting;

  const [webhook, setWebhook] = useState("");
  const [slack, setSlack] = useState("");
  const [cooldown, setCooldown] = useState<string | null>(null);
  const [editWebhook, setEditWebhook] = useState(false);
  const [editSlack, setEditSlack] = useState(false);

  const saveMut = useMutation({
    mutationFn: () =>
      patchGatewayConfig({
        alerting: {
          failover_webhook: editWebhook ? webhook : undefined,
          failover_slack: editSlack ? slack : undefined,
          cooldown: cooldown ?? alert?.cooldown,
        },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["gateway-config"] });
      setEditWebhook(false);
      setEditSlack(false);
      setWebhook("");
      setSlack("");
    },
  });

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Gateway · failover alerts
          </div>
          <h1 className="page-title">
            <GradientText as="span">Alerting</GradientText>
          </h1>
          <p className="page-sub">
            Notify when routing failovers fire. Webhook URLs are stored in-process for
            this pod; set Helm env for persistence across restarts.
          </p>
        </div>
      </header>

      {cfgQ.isLoading ? (
        <p className="muted">Loading…</p>
      ) : cfgQ.error ? (
        <Chip tone="err">{(cfgQ.error as Error).message}</Chip>
      ) : alert ? (
        <section className="panel" style={{ padding: "1.25rem" }}>
          <p className="muted small">
            Generic webhook: {alert.failover_webhook_set ? "configured" : "not set"} · Slack:{" "}
            {alert.failover_slack_set ? "configured" : "not set"}
          </p>
          <label className="field">
            <span>Failover webhook URL</span>
            <input
              type="url"
              placeholder={alert.failover_webhook_set ? "•••••••• (leave blank to keep)" : "https://…"}
              value={webhook}
              onChange={(e) => {
                setWebhook(e.target.value);
                setEditWebhook(true);
              }}
            />
          </label>
          <label className="field">
            <span>Slack webhook URL</span>
            <input
              type="url"
              placeholder={alert.failover_slack_set ? "•••••••• (leave blank to keep)" : "https://hooks.slack.com/…"}
              value={slack}
              onChange={(e) => {
                setSlack(e.target.value);
                setEditSlack(true);
              }}
            />
          </label>
          <label className="field">
            <span>Alert cooldown (duration, 0 = off)</span>
            <input
              value={cooldown ?? alert.cooldown}
              onChange={(e) => setCooldown(e.target.value)}
            />
          </label>
          <button
            type="button"
            className="btn-neon"
            disabled={saveMut.isPending}
            onClick={() => saveMut.mutate()}
          >
            {saveMut.isPending ? "Saving…" : "Save alerting"}
          </button>
          {saveMut.error ? <Chip tone="err">{(saveMut.error as Error).message}</Chip> : null}
        </section>
      ) : null}
    </div>
  );
}
