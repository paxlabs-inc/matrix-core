import { notFound } from 'next/navigation'
import { ExplorerAccount } from '@/components/matrix/explorer/explorer-account'

export default async function ExplorerAccountPage({
  params,
}: {
  params: Promise<{ did: string }>
}) {
  const { did } = await params
  const decoded = decodeURIComponent(did)
  if (!decoded.startsWith('did:')) notFound()
  return <ExplorerAccount did={decoded} />
}
