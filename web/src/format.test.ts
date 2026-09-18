import { describe, expect, it } from "vitest";
import { formatAgo, formatDuration } from "./format";

describe("formatDuration", () => {
  it.each([
    [0, "0s"],
    [-5000, "0s"],
    [45_000, "45s"],
    [215_000, "3m 35s"],
    [180_000, "3m"],
    [7_500_000, "2h 5m"],
    [7_200_000, "2h"],
    [3 * 86_400_000 + 3_600_000 * 4, "3d 4h"],
  ])("%d ms -> %s", (ms, want) => {
    expect(formatDuration(ms)).toBe(want);
  });
});

describe("formatAgo", () => {
  const now = Date.parse("2026-09-18T09:48:12Z");
  it("formats a past time", () => {
    expect(formatAgo("2026-09-18T09:44:37Z", now)).toBe("3m 35s ago");
  });
  it("handles missing and very recent times", () => {
    expect(formatAgo(undefined, now)).toBe("Never");
    expect(formatAgo("2026-09-18T09:48:11.800Z", now)).toBe("Just now");
  });
});
