import { Grid } from '@astryxdesign/core/Grid'
import { Card } from '@astryxdesign/core/Card'
import { Skeleton } from '@astryxdesign/core/Skeleton'

export default function ExplorerLoading() {
  return (
    <Grid columns={{ minWidth: 280, max: 2 }} gap={3}>
      {Array.from({ length: 4 }).map((_, i) => (
        <Card key={i} variant="muted" padding={4} minHeight={96}>
          <Skeleton width="60%" height={16} index={i} />
          <Skeleton width="90%" height={12} index={i + 1} className="mt-3" />
        </Card>
      ))}
    </Grid>
  )
}
