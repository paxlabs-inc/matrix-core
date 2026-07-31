import { Layout, LayoutContent, LayoutHeader } from '@astryxdesign/core/Layout'
import { Skeleton } from '@astryxdesign/core/Skeleton'
import { Card } from '@astryxdesign/core/Card'
import { Grid } from '@astryxdesign/core/Grid'

export default function Loading() {
  return (
    <Layout
      height="fill"
      padding={4}
      role="status"
      aria-label="Loading Matrix dashboard"
      header={
        <LayoutHeader>
          <div className="flex h-10 items-center gap-3">
            <Skeleton width={20} height={20} />
            <Skeleton width={96} height={16} />
            <div className="ml-auto flex gap-2">
              <Skeleton width={160} height={36} />
              <Skeleton width={36} height={36} />
            </div>
          </div>
        </LayoutHeader>
      }
      content={
        <LayoutContent className="space-y-6">
          <Grid columns={{ minWidth: 160, max: 4 }} gap={3}>
            {Array.from({ length: 4 }).map((_, i) => (
              <Card key={i} variant="muted" padding={4}>
                <Skeleton width={80} height={12} />
                <Skeleton width={56} height={28} className="mt-3" />
                <Skeleton width={112} height={12} className="mt-2" />
              </Card>
            ))}
          </Grid>
          <Grid columns={{ minWidth: 320, max: 2 }} gap={4}>
            {Array.from({ length: 4 }).map((_, i) => (
              <Card key={i} variant="muted" padding={4} className="space-y-3">
                <div className="flex items-center gap-3">
                  <Skeleton width={32} height={32} />
                  <div className="flex-1 space-y-2">
                    <Skeleton width={176} height={16} />
                    <Skeleton width={112} height={12} />
                  </div>
                  <Skeleton width={64} height={24} />
                </div>
                <Skeleton width="100%" height={6} />
              </Card>
            ))}
          </Grid>
        </LayoutContent>
      }
    />
  )
}
