import { notFound } from 'next/navigation'
import { ExplorerTransferDetail } from '@/components/matrix/explorer/explorer-transfer-detail'

export default async function ExplorerTransferPage({
  params,
}: {
  params: Promise<{ seq: string }>
}) {
  const { seq } = await params
  const n = Number(seq)
  if (!Number.isInteger(n) || n <= 0) notFound()
  return <ExplorerTransferDetail seq={n} />
}
