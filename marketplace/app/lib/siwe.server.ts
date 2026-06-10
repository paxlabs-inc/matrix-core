/**
 * EIP-4361 (Sign-In With Ethereum) message construction. The Worker builds
 * the message; deusd independently parses + verifies it (signature recovery,
 * nonce HMAC, domain pin), so this layout must stay in sync with
 * deus/internal/server/devauth.go parseSIWE.
 */

export interface SiweMessageInput {
  /** RFC 4501 authority, e.g. "market.paxeer.app". */
  domain: string;
  /** Full origin, e.g. "https://market.paxeer.app". */
  origin: string;
  /** EVM address being linked (0x-40-hex). */
  address: string;
  /** deusd-issued HMAC nonce. */
  nonce: string;
  /** RFC 3339 expiry (mirrors the nonce TTL). */
  expirationTime: string;
}

export const SIWE_STATEMENT =
  "Link this wallet as your Deus developer identity. This signature does not authorize any payment.";

export function buildSiweMessage(input: SiweMessageInput): string {
  const issuedAt = new Date().toISOString();
  return [
    `${input.domain} wants you to sign in with your Ethereum account:`,
    input.address,
    "",
    SIWE_STATEMENT,
    "",
    `URI: ${input.origin}`,
    "Version: 1",
    "Chain ID: 125",
    `Nonce: ${input.nonce}`,
    `Issued At: ${issuedAt}`,
    `Expiration Time: ${input.expirationTime}`,
  ].join("\n");
}

export const EVM_ADDRESS_RE = /^0x[0-9a-fA-F]{40}$/;
