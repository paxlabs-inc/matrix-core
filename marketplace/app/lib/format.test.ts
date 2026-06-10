import { describe, expect, it } from "vitest";
import {
  formatCount,
  formatPax,
  formatQuality,
  formatUptime,
  paxToWei,
  shortAddress,
  statusTone,
  titleCase,
  weiToPax,
} from "./format";

describe("weiToPax / paxToWei", () => {
  it("converts whole PAX round-trip", () => {
    expect(weiToPax("1000000000000000000")).toBe(1);
    expect(paxToWei("1")).toBe("1000000000000000000");
  });
  it("handles fractional PAX", () => {
    expect(paxToWei("0.0008")).toBe("800000000000000");
    expect(weiToPax("800000000000000")).toBeCloseTo(0.0008, 9);
  });
  it("is safe on empty / invalid input", () => {
    expect(weiToPax("")).toBe(0);
    expect(weiToPax(undefined)).toBe(0);
    expect(paxToWei("")).toBe("0");
    expect(paxToWei("-5")).toBe("0");
  });
});

describe("formatPax", () => {
  it("renders a symbol by default", () => {
    expect(formatPax("1000000000000000000")).toBe("1 PAX");
  });
  it("can omit the symbol", () => {
    expect(formatPax("1000000000000000000", { withSymbol: false })).toBe("1");
  });
  it("uses exponential for sub-0.0001 values", () => {
    expect(formatPax("800000000000000")).toContain("PAX");
  });
  it("renders 0 cleanly", () => {
    expect(formatPax("0")).toBe("0 PAX");
  });
});

describe("formatCount", () => {
  it("formats thousands and millions", () => {
    expect(formatCount(950)).toBe("950");
    expect(formatCount(1500)).toBe("1.5K");
    expect(formatCount(2_400_000)).toBe("2.4M");
    expect(formatCount(0)).toBe("0");
  });
});

describe("shortAddress", () => {
  it("truncates long addresses", () => {
    expect(shortAddress("0x1234567890abcdef1234567890abcdef12345678")).toBe("0x1234…5678");
  });
  it("returns empty for nullish", () => {
    expect(shortAddress(undefined)).toBe("");
  });
});

describe("formatUptime / formatQuality", () => {
  it("formats basis points", () => {
    expect(formatUptime(9995)).toBe("99.9%");
    expect(formatUptime(10000)).toBe("100%");
    expect(formatUptime(undefined)).toBe("—");
  });
  it("formats a 0..1 quality score to 0..100", () => {
    expect(formatQuality("0.96")).toBe("96");
    expect(formatQuality(undefined)).toBe("—");
  });
});

describe("statusTone / titleCase", () => {
  it("maps statuses to tones", () => {
    expect(statusTone("active")).toBe("active");
    expect(statusTone("draft")).toBe("draft");
    expect(statusTone("delisted")).toBe("delisted");
    expect(statusTone("whatever")).toBe("neutral");
  });
  it("title-cases snake/kebab", () => {
    expect(titleCase("always_warm")).toBe("Always Warm");
  });
});
