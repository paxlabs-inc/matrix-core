import { notFound } from 'next/navigation'
import { ExplorerBatchDetail } from '@/components/matrix/explorer/explorer-batch-detail'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export default async function ExplorerBatchPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params
  if (!UUID_RE.test(id)) notFound()
  return <ExplorerBatchDetail id={id} />
}
