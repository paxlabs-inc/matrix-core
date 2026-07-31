'use client'

/**
 * LayerX explorer hooks — queries over the layerxd public read surface plus
 * the live SSE stream. All reads ride the same-origin /layerx-api proxy, so
 * they inherit the app's session gate; nothing here can move value.
 */
import { useEffect, useRef } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import {
  getLayerXBatch,
  getLayerXBatches,
  getLayerXInfo,
  getLayerXReceipt,
  getLayerXSupply,
  getLayerXTransfers,
  layerxStreamUrl,
  type LayerXBatch,
  type LayerXBatchesResponse,
  type LayerXInfo,
  type LayerXReceipt,
  type LayerXSupply,
  type LayerXTransfer,
  type LayerXTransfersResponse,
} from '@/lib/layerx/explorer'
import { getLayerXAccount, layerxApiBase, type LayerXAccount } from '@/lib/layerx/client'

/** True when the LayerX surface is configured for this deployment. */
export function layerxConfigured(): boolean {
  return layerxApiBase() !== null
}

export function useLayerXInfo() {
  return useQuery<LayerXInfo>({
    queryKey: qk.layerxInfo(),
    queryFn: ({ signal }) => getLayerXInfo(signal),
    enabled: layerxConfigured(),
    staleTime: 5 * 60_000,
  })
}

export function useLayerXSupply() {
  return useQuery<LayerXSupply>({
    queryKey: qk.layerxSupply(),
    queryFn: ({ signal }) => getLayerXSupply(signal),
    enabled: layerxConfigured(),
    staleTime: 15_000,
    refetchInterval: 30_000,
  })
}

/** Keyset-paginated transfer feed, optionally scoped to one DID. */
export function useLayerXTransfers(did?: string, pageSize = 25) {
  return useInfiniteQuery<LayerXTransfersResponse>({
    queryKey: qk.layerxTransfers(did),
    queryFn: ({ pageParam, signal }) =>
      getLayerXTransfers({ did, limit: pageSize, before: pageParam as number }, signal),
    initialPageParam: 0,
    getNextPageParam: (last) =>
      last.next_before && last.next_before > 0 ? last.next_before : undefined,
    enabled: layerxConfigured(),
    staleTime: 10_000,
  })
}

export function useLayerXReceipt(seq: number | null) {
  return useQuery<LayerXReceipt>({
    queryKey: qk.layerxReceipt(seq ?? -1),
    queryFn: ({ signal }) => getLayerXReceipt(seq as number, signal),
    enabled: layerxConfigured() && seq !== null && seq > 0,
    staleTime: 30_000,
  })
}

export function useLayerXBatches(pageSize = 25) {
  return useInfiniteQuery<LayerXBatchesResponse>({
    queryKey: qk.layerxBatches(),
    queryFn: ({ pageParam, signal }) => getLayerXBatches(pageSize, pageParam as number, signal),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.count === last.limit ? last.offset + last.limit : undefined),
    enabled: layerxConfigured(),
    staleTime: 15_000,
  })
}

export function useLayerXBatch(id: string | null) {
  return useQuery<LayerXBatch>({
    queryKey: qk.layerxBatch(id ?? ''),
    queryFn: ({ signal }) => getLayerXBatch(id as string, signal),
    enabled: layerxConfigured() && Boolean(id),
    staleTime: 30_000,
  })
}

/** Public account view (balance, escrow, payout address, history). */
export function useLayerXAccountView(did: string | null) {
  return useQuery<LayerXAccount>({
    queryKey: qk.layerxAccount(did ?? ''),
    queryFn: ({ signal }) => getLayerXAccount(did as string, signal),
    enabled: layerxConfigured() && Boolean(did),
    staleTime: 15_000,
  })
}

export interface LayerXAnchorEvent {
  batch_id: string
  root: string
  anchor_tx: string
  transfer_count: number
}

interface StreamHandlers {
  onTransfer?: (t: LayerXTransfer) => void
  onAnchor?: (a: LayerXAnchorEvent) => void
  enabled?: boolean
}

/**
 * Live tail of the LayerX sequencer: `transfer` events as payments are
 * accepted, `anchor` events as batches settle on Paxeer. The browser's
 * EventSource auto-reconnects with Last-Event-ID, and layerxd replays any
 * missed transfers, so the feed never silently drops a payment.
 */
export function useLayerXStream({ onTransfer, onAnchor, enabled = true }: StreamHandlers) {
  const handlers = useRef({ onTransfer, onAnchor })
  handlers.current = { onTransfer, onAnchor }

  useEffect(() => {
    if (!enabled) return
    const url = layerxStreamUrl()
    if (!url) return
    const es = new EventSource(url)
    const onTransferEvt = (ev: MessageEvent<string>) => {
      try {
        handlers.current.onTransfer?.(JSON.parse(ev.data) as LayerXTransfer)
      } catch {
        /* malformed frame: skip */
      }
    }
    const onAnchorEvt = (ev: MessageEvent<string>) => {
      try {
        handlers.current.onAnchor?.(JSON.parse(ev.data) as LayerXAnchorEvent)
      } catch {
        /* malformed frame: skip */
      }
    }
    es.addEventListener('transfer', onTransferEvt)
    es.addEventListener('anchor', onAnchorEvt)
    return () => {
      es.removeEventListener('transfer', onTransferEvt)
      es.removeEventListener('anchor', onAnchorEvt)
      es.close()
    }
  }, [enabled])
}
