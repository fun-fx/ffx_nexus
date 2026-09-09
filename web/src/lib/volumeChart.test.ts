import { describe, expect, it } from "vitest";
import { parseVolumeChartKind, seriesHasVolume, toUPlotData } from "./volumeChart";

describe("toUPlotData", () => {
  const buckets = [
    { timestamp: "2026-01-01T00:00:00Z", ok: 5, err: 1 },
    { timestamp: "2026-01-01T00:01:00Z", ok: 3, err: 0 },
  ];

  it("aligns unix seconds then visible series", () => {
    const data = toUPlotData(buckets, true, true);
    expect(data).toHaveLength(3);
    expect(data[0]).toEqual([
      Math.floor(Date.parse("2026-01-01T00:00:00Z") / 1000),
      Math.floor(Date.parse("2026-01-01T00:01:00Z") / 1000),
    ]);
    expect(data[1]).toEqual([5, 3]);
    expect(data[2]).toEqual([1, 0]);
  });

  it("omits error series when filtered off", () => {
    const data = toUPlotData(buckets, true, false);
    expect(data).toHaveLength(2);
    expect(data[1]).toEqual([5, 3]);
  });
});

describe("seriesHasVolume", () => {
  it("is false when every bin is zero", () => {
    expect(
      seriesHasVolume(
        [{ timestamp: "2026-01-01T00:00:00Z", ok: 0, err: 0 }],
        true,
        true,
      ),
    ).toBe(false);
  });

  it("respects status filters", () => {
    const buckets = [{ timestamp: "2026-01-01T00:00:00Z", ok: 0, err: 2 }];
    expect(seriesHasVolume(buckets, true, false)).toBe(false);
    expect(seriesHasVolume(buckets, false, true)).toBe(true);
  });
});

describe("parseVolumeChartKind", () => {
  it("defaults to line", () => {
    expect(parseVolumeChartKind(null)).toBe("line");
    expect(parseVolumeChartKind("line")).toBe("line");
    expect(parseVolumeChartKind("bar")).toBe("bar");
  });
});
