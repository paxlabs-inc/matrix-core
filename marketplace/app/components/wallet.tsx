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
  const busy = fetcher.state !== "idle";

  async function connect() {
    let address: string | undefined;
    if (typeof window !== "undefined" && window.ethereum) {
      try {
        const accounts = (await window.ethereum.request({
          method: "eth_requestAccounts",
        })) as string[];
        address = accounts?.[0];
      } catch {
        address = undefined;
      }
    }
    if (!address && allowDev) address = DEV_WALLET;
    if (!address) {
      alert("No EVM wallet detected. Install a wallet extension to connect.");
      return;
    }
    fetcher.submit(
      { intent: "link", address },
      { method: "post", action: "/api/wallet" }
    );
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
