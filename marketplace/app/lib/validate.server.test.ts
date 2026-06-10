import { describe, expect, it } from "vitest";
import {
  emailSchema,
  evmAddressSchema,
  listingSchema,
  parseForm,
  siweLinkSchema,
  tryItSchema,
} from "./validate.server";

describe("emailSchema", () => {
  it("accepts a normal email and rejects junk", () => {
    expect(emailSchema.safeParse("dev@studio.dev").success).toBe(true);
    expect(emailSchema.safeParse("not-an-email").success).toBe(false);
    expect(emailSchema.safeParse("a@" + "b".repeat(260) + ".com").success).toBe(false);
  });
});

describe("evmAddressSchema", () => {
  it("requires 0x + 40 hex", () => {
    expect(evmAddressSchema.safeParse("0x" + "ab".repeat(20)).success).toBe(true);
    expect(evmAddressSchema.safeParse("0x123").success).toBe(false);
    expect(evmAddressSchema.safeParse("ab".repeat(21)).success).toBe(false);
  });
});

describe("siweLinkSchema", () => {
  it("caps message size and shapes the signature", () => {
    const ok = siweLinkSchema.safeParse({ message: "m", signature: "0x" + "ab".repeat(65) });
    expect(ok.success).toBe(true);
    expect(
      siweLinkSchema.safeParse({ message: "m".repeat(5000), signature: "0x" + "ab".repeat(65) })
        .success
    ).toBe(false);
    expect(siweLinkSchema.safeParse({ message: "m", signature: "0xzz" }).success).toBe(false);
  });
});

describe("tryItSchema", () => {
  it("bounds units and caps args", () => {
    const form = new FormData();
    form.set("operation", "forecast");
    form.set("units", "3");
    const parsed = parseForm(tryItSchema, form, ["operation", "units", "args"]);
    expect(parsed.ok).toBe(true);
    if (parsed.ok) expect(parsed.data.units).toBe(3);

    form.set("units", "10000");
    expect(parseForm(tryItSchema, form, ["operation", "units", "args"]).ok).toBe(false);

    form.set("units", "1");
    form.set("args", "x".repeat(20_000));
    expect(parseForm(tryItSchema, form, ["operation", "units", "args"]).ok).toBe(false);
  });

  it("rejects hostile operation names", () => {
    const form = new FormData();
    form.set("operation", "../../etc/passwd");
    form.set("units", "1");
    expect(parseForm(tryItSchema, form, ["operation", "units", "args"]).ok).toBe(false);
  });
});

describe("listingSchema", () => {
  const base = {
    display_name: "My Service",
    slug: "my-service",
    kind: "data",
    mode: "proxy",
    summary: "Does things.",
    description: "",
    proxy_url: "https://api.example.com",
    tags: "a,b",
    operations_json: "[]",
  };

  it("accepts a well-formed listing", () => {
    expect(listingSchema.safeParse(base).success).toBe(true);
  });

  it("rejects a bad slug, bad proxy url, and missing name", () => {
    expect(listingSchema.safeParse({ ...base, slug: "Bad Slug!" }).success).toBe(false);
    expect(listingSchema.safeParse({ ...base, proxy_url: "notaurl" }).success).toBe(false);
    expect(listingSchema.safeParse({ ...base, display_name: "" }).success).toBe(false);
  });

  it("allows empty slug (server derives it) and empty proxy_url", () => {
    expect(listingSchema.safeParse({ ...base, slug: "", proxy_url: "" }).success).toBe(true);
  });
});
