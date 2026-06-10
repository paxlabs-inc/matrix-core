import { describe, expect, it, vi } from "vitest";
import { cachedJson } from "./cache.server";

describe("cachedJson (in-memory fallback)", () => {
  it("returns the cached value on a repeat read", async () => {
    const key = "k2:" + Date.now();
    const fetcher = vi.fn(async () => ({ v: 42 }));
    const first = await cachedJson({}, key, 60, fetcher);
    const second = await cachedJson({}, key, 60, fetcher);
    expect(first).toEqual({ v: 42 });
    expect(second).toEqual({ v: 42 });
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it("serves the stale copy when the fetcher throws (stale-on-error)", async () => {
    const key = "k3:" + Date.now();
    let calls = 0;
    const fetcher = vi.fn(async () => {
      calls += 1;
      if (calls > 1) throw new Error("backend down");
      return { v: "good" };
    });
    await cachedJson({}, key, 0, fetcher); // ttl 0 → immediately stale
    const second = await cachedJson({}, key, 0, fetcher);
    expect(second).toEqual({ v: "good" });
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it("rethrows when there is no stale copy to fall back to", async () => {
    const fetcher = vi.fn(async () => {
      throw new Error("backend down");
    });
    await expect(cachedJson({}, "k4:" + Date.now(), 60, fetcher)).rejects.toThrow("backend down");
  });
});
