import type { VolumeBucket } from "../api";

export type VolumeChartKind = "line" | "bar";

export type UPlotAligned = [number[], ...(number[][])];

// toUPlotData converts dense volume buckets into uPlot's aligned columns:
// timestamps (unix seconds) then one y-series per visible status.
export function toUPlotData(
  buckets: VolumeBucket[],
  statusOk: boolean,
  statusErr: boolean,
): UPlotAligned {
  const xs = buckets.map((b) => {
    const t = new Date(b.timestamp).getTime();
    return Number.isFinite(t) ? Math.floor(t / 1000) : 0;
  });
  const series: number[][] = [xs];
  if (statusOk) series.push(buckets.map((b) => b.ok));
  if (statusErr) series.push(buckets.map((b) => b.err));
  return series as UPlotAligned;
}

export function seriesHasVolume(
  buckets: VolumeBucket[],
  statusOk: boolean,
  statusErr: boolean,
): boolean {
  return buckets.some((b) => (statusOk && b.ok > 0) || (statusErr && b.err > 0));
}

export function parseVolumeChartKind(raw: string | null): VolumeChartKind {
  return raw === "bar" ? "bar" : "line";
}
