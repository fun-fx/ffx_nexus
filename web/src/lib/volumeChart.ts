import type { VolumeBucket } from "../api";

export type VolumeChartKind = "line" | "bar";

export type UPlotAligned = [number[], ...(number[][])];

export type ChartSeries = {
  label: string;
  colorVar: string;
  values: number[];
};

export function timestampsToUnix(timestamps: string[]): number[] {
  return timestamps.map((ts) => {
    const t = new Date(ts).getTime();
    return Number.isFinite(t) ? Math.floor(t / 1000) : 0;
  });
}

export function toAlignedUPlotData(
  timestamps: string[],
  series: { values: number[] }[],
): UPlotAligned {
  const xs = timestampsToUnix(timestamps);
  const cols: number[][] = [xs];
  for (const s of series) {
    cols.push(s.values);
  }
  return cols as UPlotAligned;
}

// toUPlotData converts dense volume buckets into uPlot's aligned columns:
// timestamps (unix seconds) then one y-series per visible status.
export function toUPlotData(
  buckets: VolumeBucket[],
  statusOk: boolean,
  statusErr: boolean,
): UPlotAligned {
  const series: { values: number[] }[] = [];
  if (statusOk) series.push({ values: buckets.map((b) => b.ok) });
  if (statusErr) series.push({ values: buckets.map((b) => b.err) });
  return toAlignedUPlotData(
    buckets.map((b) => b.timestamp),
    series,
  );
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

export function parseCSVParam(raw: string | null): string[] {
  if (!raw) return [];
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

export function toggleCSVValue(current: string[], value: string): string[] {
  if (current.includes(value)) {
    return current.filter((v) => v !== value);
  }
  return [...current, value];
}
