import { useEffect, useRef } from "react";
import uPlot from "uplot";
import {
  toAlignedUPlotData,
  type ChartSeries,
  type VolumeChartKind,
} from "../../lib/volumeChart";

type Props = {
  timestamps: string[];
  series: ChartSeries[];
  kind: VolumeChartKind;
  testId?: string;
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

const FALLBACKS: Record<string, string> = {
  "--ok": "#4ade80",
  "--err": "#fb7185",
  "--accent": "#ec4899",
  "--accent-2": "#22d3ee",
  "--accent-3": "#a855f7",
  "--warn": "#facc15",
  "--info": "#60a5fa",
};

export function TimeSeriesChart({
  timestamps,
  series,
  kind,
  testId = "time-series-chart",
}: Props) {
  const rootRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);

  useEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    if (import.meta.env.MODE === "test") return;

    const muted = token(root, "--muted", "#888");
    const border = token(root, "--border", "#333");

    const seriesDefs: uPlot.Series[] = [{}];
    for (const s of series) {
      const color = token(root, s.colorVar, FALLBACKS[s.colorVar] ?? "#888");
      seriesDefs.push({
        label: s.label,
        stroke: color,
        fill: withAlpha(color, "59"),
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
      toAlignedUPlotData(timestamps, series),
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
  }, [timestamps, series, kind]);

  return <div ref={rootRef} className="time-series-chart" data-testid={testId} />;
}
