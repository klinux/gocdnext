import { describe, expect, it } from "vitest";
import { formatBytes, formatDurationSeconds, formatRelative } from "./format";

describe("formatDurationSeconds", () => {
  it("handles sub-second values", () => {
    expect(formatDurationSeconds(0)).toBe("0s");
    expect(formatDurationSeconds(0.5)).toBe("< 1s");
  });

  it("formats seconds and minutes", () => {
    expect(formatDurationSeconds(7)).toBe("7s");
    expect(formatDurationSeconds(90)).toBe("1m 30s");
    expect(formatDurationSeconds(3599)).toBe("59m 59s");
  });

  it("formats hours+minutes but drops trailing seconds", () => {
    expect(formatDurationSeconds(3600)).toBe("1h 0m");
    expect(formatDurationSeconds(3725)).toBe("1h 2m");
  });

  it("returns em-dash for nullish input", () => {
    expect(formatDurationSeconds(null)).toBe("—");
    expect(formatDurationSeconds(undefined)).toBe("—");
  });
});

describe("formatRelative", () => {
  const now = new Date("2026-04-17T12:00:00Z");

  it("returns 'just now' within 10 seconds", () => {
    const d = new Date("2026-04-17T11:59:55Z");
    expect(formatRelative(d, now)).toBe("just now");
  });

  it("minutes, hours, days", () => {
    expect(formatRelative(new Date("2026-04-17T11:58:00Z"), now)).toBe(
      "2 minutes ago",
    );
    expect(formatRelative(new Date("2026-04-17T09:00:00Z"), now)).toBe(
      "3 hours ago",
    );
    expect(formatRelative(new Date("2026-04-15T12:00:00Z"), now)).toBe(
      "2 days ago",
    );
  });

  it("returns em-dash for null/undefined", () => {
    expect(formatRelative(null, now)).toBe("—");
  });
});

describe("formatBytes", () => {
  it("handles zero and nullish", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(null)).toBe("—");
    expect(formatBytes(undefined)).toBe("—");
  });

  it("uses KB up to 1024 and scales units", () => {
    expect(formatBytes(500)).toBe("500 B");
    expect(formatBytes(2048)).toBe("2.0 KB");
    expect(formatBytes(5 * 1024 * 1024)).toBe("5.0 MB");
    expect(formatBytes(2 * 1024 * 1024 * 1024)).toBe("2.0 GB");
  });

  it("rounds integer-style for values >= 10 in the unit", () => {
    // 12.3 KB → "12 KB" (one-decimal only for single-digit
    // values — keeps the column narrow in the caches table).
    expect(formatBytes(12500)).toBe("12 KB");
  });
});
