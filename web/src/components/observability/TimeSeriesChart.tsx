import { useEffect, useRef } from "react";
import uPlot from "uplot";
import type { VolumeBucket } from "../../api";
import { toUPlotData, type VolumeChartKind } from "../../lib/volumeChart";

type Props = {
  buckets: VolumeBucket[];
  statusOk: boolean;
  statusErr: boolean;
  kind: VolumeChartKind;
};

function token(el: HTMLElement, name: string, fallback: string): string {
  const v = getComputedStyle(el).getPropertyValue(name).trim();
  return v || fallback;
}

function withAlpha(color: string, alpha: string): string {
  if (color.startsWith("#") && (color.length === 7 || color.length === 4)) {
    return color.length === 7 ? `${color}${alpha}` : color;
  }
  return color;
}

export function TimeSeriesChart({ buckets, statusOk, statusErr, kind }: Props) {
  const rootRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);

  useEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    if (import.meta.env.MODE === "test") return;

    const okColor = token(root, "--ok", "#4ade80");
    const errColor = token(root, "--err", "#fb7185");
    const muted = token(root, "--muted", "#888");
    const border = token(root, "--border", "#333");

    const seriesDefs: uPlot.Series[] = [{}];
    if (statusOk) {
      seriesDefs.push({
        label: "Success",
        stroke: okColor,
        fill: withAlpha(okColor, "59"),
        width: 1.5,
        points: { show: false },
        paths: kind === "bar" ? uPlot.paths.bars!({ size: [0.7, 64] }) : undefined,
      });
    }
    if (statusErr) {
      seriesDefs.push({
        label: "Error",
        stroke: errColor,
        fill: withAlpha(errColor, "59"),
        width: 1.5,
        points: { show: false },
        paths: kind === "bar" ? uPlot.paths.bars!({ size: [0.7, 64] }) : undefined,
      });
    }

    const width = Math.max(120, root.clientWidth);
    const height = Math.max(160, root.clientHeight);

    const plot = new uPlot(
      {
        width,
        height,
        cursor: { show: true, drag: { x: false, y: false } },
        legend: { show: false },
        scales: { x: { time: true } },
        series: seriesDefs,
        axes: [
          {
            stroke: muted,
            ticks: { stroke: border },
            grid: { stroke: border },
          },
          {
            stroke: muted,
            ticks: { stroke: border },
            grid: { stroke: border },
            size: 44,
          },
        ],
      },
      toUPlotData(buckets, statusOk, statusErr),
      root,
    );
    plotRef.current = plot;

    const ro = new ResizeObserver(() => {
      const w = Math.max(120, root.clientWidth);
      const h = Math.max(160, root.clientHeight);
      plot.setSize({ width: w, height: h });
    });
    ro.observe(root);

    return () => {
      ro.disconnect();
      plot.destroy();
      plotRef.current = null;
    };
  }, [buckets, statusOk, statusErr, kind]);

  return <div ref={rootRef} className="time-series-chart" data-testid="volume-chart" />;
}
