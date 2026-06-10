import { z } from "zod";

/**
 * Schema validation + length caps for every user-supplied action input.
 * The Go backend re-validates manifests; this layer exists to reject junk
 * and oversized payloads at the edge before they consume backend cycles.
 */

export const EVM_ADDRESS = /^0x[0-9a-fA-F]{40}$/;

export const emailSchema = z.email().trim().max(254);

export const evmAddressSchema = z
  .string()
  .trim()
  .regex(EVM_ADDRESS, "Enter a valid EVM address (0x…40 hex chars).");

export const siweLinkSchema = z.object({
  message: z.string().min(1).max(4096),
  signature: z
    .string()
    .trim()
    .regex(/^0x[0-9a-fA-F]{130}$/, "Malformed wallet signature."),
});

export const tryItSchema = z.object({
  operation: z
    .string()
    .trim()
    .min(1, "Pick an operation.")
    .max(64)
    .regex(/^[A-Za-z0-9_.-]+$/, "Invalid operation name."),
  units: z.coerce.number().int().min(1).max(999),
  /** Raw JSON text; parsed separately so we can cap size before parsing.
   * Optional because quote-only submissions don't carry arguments. */
  args: z.string().max(16_384, "Arguments are too large (16 KB max).").default("{}"),
});

export const operationRowSchema = z.object({
  name: z.string().trim().min(1).max(64),
  method: z.string().trim().max(8).default("POST"),
  description: z.string().max(280).default(""),
  unit: z.string().max(32).default(""),
  price: z.string().max(32).default(""),
  inputSchema: z.string().max(16_384).default(""),
  outputSchema: z.string().max(16_384).default(""),
});

export const listingSchema = z.object({
  display_name: z.string().trim().min(1, "Give your service a name.").max(80),
  slug: z
    .string()
    .trim()
    .max(80)
    .regex(/^[a-z0-9][a-z0-9-]*$/, "Slug must be lowercase letters, digits, and dashes.")
    .or(z.literal("")),
  kind: z.enum(["data", "agent"]),
  mode: z.enum(["proxy", "hosted"]),
  summary: z.string().trim().max(280, "Keep the summary under 280 characters."),
  description: z.string().trim().max(4_000),
  proxy_url: z.url().max(512).or(z.literal("")),
  tags: z.string().max(512),
  operations_json: z.string().max(65_536),
});

export const payoutSchema = z.object({
  payout_address: evmAddressSchema,
});

/** First human-readable issue from a failed parse. */
export function firstIssue(error: z.ZodError): string {
  return error.issues[0]?.message ?? "Invalid input.";
}

export type ParseFormResult<T> = { ok: true; data: T } | { ok: false; error: string };

/** Parse a FormData subset against a schema; returns data or an error text. */
export function parseForm<S extends z.ZodType>(
  schema: S,
  form: FormData,
  keys: string[]
): ParseFormResult<z.infer<S>> {
  const raw: Record<string, unknown> = {};
  for (const key of keys) {
    const v = form.get(key);
    // Missing fields become "" so required-ness is enforced by min(1)
    // where it matters instead of failing on absent optional inputs.
    raw[key] = v === null ? "" : String(v);
  }
  const result = schema.safeParse(raw);
  if (!result.success) return { ok: false, error: firstIssue(result.error) };
  return { ok: true, data: result.data };
}
