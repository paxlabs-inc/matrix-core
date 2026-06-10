import { describe, expect, it } from "vitest";
import {
  commitSession,
  getSession,
  isDevAuth,
  isProduction,
  rotateSession,
} from "./auth.server";

const DEV_ENV = {
  ENVIRONMENT: "development",
  MARKETPLACE_DEV_AUTH: "1",
  SESSION_SECRET: "dev-local-marketplace-secret",
};

describe("isProduction", () => {
  it("treats an unset ENVIRONMENT as production (fail closed)", () => {
    expect(isProduction({})).toBe(true);
    expect(isProduction({ ENVIRONMENT: "production" })).toBe(true);
    expect(isProduction({ ENVIRONMENT: "staging" })).toBe(true);
  });

  it("only 'development' is non-production", () => {
    expect(isProduction({ ENVIRONMENT: "development" })).toBe(false);
  });
});

describe("isDevAuth", () => {
  it("requires the explicit opt-in flag AND a development environment", () => {
    expect(isDevAuth(DEV_ENV)).toBe(true);
  });

  it("is off in production even when the flag is set", () => {
    expect(isDevAuth({ ENVIRONMENT: "production", MARKETPLACE_DEV_AUTH: "1" })).toBe(false);
  });

  it("is off when the flag is unset, even without Supabase config", () => {
    expect(isDevAuth({ ENVIRONMENT: "development" })).toBe(false);
    expect(isDevAuth({})).toBe(false);
  });
});

describe("session secret enforcement", () => {
  it("throws in production when SESSION_SECRET is missing", () => {
    const req = new Request("https://deus.example/");
    expect(() => getSession(req, { ENVIRONMENT: "production" })).toThrow(/SESSION_SECRET/);
  });

  it("throws in production when SESSION_SECRET is too short", () => {
    const req = new Request("https://deus.example/");
    expect(() =>
      getSession(req, { ENVIRONMENT: "production", SESSION_SECRET: "short" })
    ).toThrow(/SESSION_SECRET/);
  });

  it("falls back to the dev secret only outside production", async () => {
    const req = new Request("https://deus.example/");
    const session = await getSession(req, { ENVIRONMENT: "development" });
    expect(session).toBeDefined();
  });
});

describe("rotateSession", () => {
  it("issues a fresh session preserving wallet + developer token only", async () => {
    const req = new Request("https://deus.example/");
    const old = await getSession(req, DEV_ENV);
    old.set("user", JSON.stringify({ id: "u1", did: "did:matrix:x:y" }));
    old.set("wallet", "0xabc0000000000000000000000000000000000abc");
    old.set("developerToken", "tok123");

    const fresh = await rotateSession(DEV_ENV, old);
    expect(fresh.get("user")).toBeUndefined();
    expect(fresh.get("wallet")).toBe("0xabc0000000000000000000000000000000000abc");
    expect(fresh.get("developerToken")).toBe("tok123");

    const cookie = await commitSession(DEV_ENV, fresh);
    expect(cookie).toContain("deus_session=");
  });
});
