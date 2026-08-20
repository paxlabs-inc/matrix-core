import type { Metadata } from 'next'
import { WorkforceCommandCenter } from '@/components/centra/workforce/workforce-command-center'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata: Metadata = {
  title: 'Workforce',
  description: 'Owner command center for Centra AI Workforce operations.',
}

export default function WorkforcePage() {
  return <WorkforceCommandCenter />
}
