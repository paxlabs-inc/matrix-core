import type { Metadata } from 'next'
import { ExplorerShell } from '@/components/matrix/explorer/explorer-shell'

/**
 * LayerX explorer — the human-readable window onto the agent settlement
 * layer. Authenticated like the rest of the app (LayerX is a Centra AI agent
 * protocol; the explorer is for its humans).
 */
export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata: Metadata = {
  title: 'LayerX Explorer',
}

export default function ExplorerLayout({ children }: { children: React.ReactNode }) {
  return <ExplorerShell>{children}</ExplorerShell>
}
