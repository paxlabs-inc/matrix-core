'use client'

/**
 * LayerX hooks — read-only public account view for the agent's DID.
 * The write lanes are DID-signed by the agent daemon and never touched
 * from the browser; the UI only observes.
 */
import { useQuery } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import { getLayerXAccount, layerxApiBase, type LayerXAccount } from '@/lib/layerx/client'

/** True when a LayerX API origin is configured for this build. */
export function layerxEnabled(): boolean {
  return layerxApiBase() !== null
}

/** The agent's LayerX account (balances, escrow, bound EVM, history). */
export function useLayerXAccount(did: string | null) {
  return useQuery<LayerXAccount>({
    queryKey: qk.layerxAccount(did ?? ''),
    queryFn: ({ signal }) => getLayerXAccount(did as string, signal),
    enabled: Boolean(did) && layerxEnabled(),
    staleTime: 20_000,
  })
}
