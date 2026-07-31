import type { Metadata } from 'next'
import { WorkforceCommandCenter } from '@/components/matrix/workforce/workforce-command-center'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata: Metadata = {
  title: 'Workforce',
  description: 'Owner command center for Matrix Workforce operations.',
}

export default function WorkforcePage() {
  return <WorkforceCommandCenter />
}
