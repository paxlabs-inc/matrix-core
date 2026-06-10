import { useEffect, useState } from "react";
import { useFetcher } from "react-router";
import { Wallet, X } from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import { shortAddress } from "@/lib/format";
import { Spinner } from "@/components/ui";

declare global {
  interface Window {
    ethereum?: {
      request: (args: { method: string; params?: unknown[] }) => Promise<unknown>;
    };
  }
}

/** Deterministic dev wallet used when no injected provider is present. */
const DEV_WALLET = "0xd3a7b9c2e4f60182a3b4c5d6e7f8091a2b3c4d5e";

export function WalletButton({
  wallet,
  allowDev = false,
  size = "default",
  className,
}: {
  wallet: string | null;
  allowDev?: boolean;
  size?: "default" | "sm" | "lg";
  className?: string;
}) {
  const fetcher = useFetcher();
  const [signing, setSigning] = useState(false);
  const busy = fetcher.state !== "idle" || signing;

  const serverError = (fetcher.data as { error?: string } | undefined)?.error;
  useEffect(() => {
    if (serverError) alert(serverError);
  }, [serverError]);

  async function connect() {
    const provider = typeof window !== "undefined" ? window.ethereum : undefined;

    if (!provider) {
      if (allowDev) {
        // Local-dev fallback (no signature, server-gated to dev auth only).
        fetcher.submit(
          { intent: "dev-link", address: DEV_WALLET },
          { method: "post", action: "/api/wallet" }
        );
        return;
      }
      alert("No EVM wallet detected. Install a wallet extension to connect.");
      return;
    }

    setSigning(true);
    try {
      const accounts = (await provider.request({
        method: "eth_requestAccounts",
      })) as string[];
      const address = accounts?.[0];
      if (!address) return;

      // 1. Ask the server for the SIWE message (it fetches a deusd nonce).
      const prepareBody = new URLSearchParams({ intent: "prepare", address });
      const prepared = (await (
        await fetch("/api/wallet", { method: "POST", body: prepareBody })
      ).json()) as { message?: string; error?: string };
      if (!prepared.message) {
        alert(prepared.error ?? "Wallet linking is unavailable right now.");
        return;
      }

      // 2. personal_sign the message (hex-encoded per EIP-191 conventions).
      const hexMessage =
        "0x" +
        Array.from(new TextEncoder().encode(prepared.message))
          .map((b) => b.toString(16).padStart(2, "0"))
          .join("");
      const signature = (await provider.request({
        method: "personal_sign",
        params: [hexMessage, address],
      })) as string;

      // 3. Server forwards to deusd, which recovers + verifies the signer.
      fetcher.submit(
        { intent: "link", message: prepared.message, signature },
        { method: "post", action: "/api/wallet" }
      );
    } catch {
      // User rejected the wallet prompt or the provider errored; no-op.
    } finally {
      setSigning(false);
    }
  }

  function disconnect() {
    fetcher.submit({ intent: "unlink" }, { method: "post", action: "/api/wallet" });
  }

  if (wallet) {
    return (
      <span
        className={
          "inline-flex items-center gap-2 rounded-full bg-secondary py-2 pr-2 pl-3 text-sm text-foreground " +
          (className ?? "")
        }
      >
        <span className="size-1.5 rounded-full bg-[color:var(--success)]" />
        <span className="mono text-xs">{shortAddress(wallet)}</span>
        <button
          type="button"
          onClick={disconnect}
          disabled={busy}
          aria-label="Disconnect wallet"
          className="flex size-5 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-background hover:text-foreground"
        >
          {busy ? <Spinner className="size-3" /> : <X className="size-3.5" />}
        </button>
      </span>
    );
  }

  return (
    <SmoothButton
      type="button"
      variant="default"
      size={size}
      onClick={connect}
      disabled={busy}
      className={className}
    >
      {busy ? <Spinner /> : <Wallet className="size-4" />}
      Connect wallet
    </SmoothButton>
  );
}
