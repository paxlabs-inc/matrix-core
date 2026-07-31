import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { Card } from '@astryxdesign/core/Card'
import { Skeleton } from '@astryxdesign/core/Skeleton'

export default function LoginLoading() {
  return (
    <Layout
      height="fill"
      padding={6}
      className="bg-background min-h-screen"
      content={
        <Center>
          <Card variant="muted" padding={5} width="100%" maxWidth={384}>
            <div className="flex flex-col gap-4">
              <Skeleton width={128} height={40} className="mx-auto" />
              <Skeleton width="100%" height={40} />
              <Skeleton width="100%" height={40} />
            </div>
          </Card>
        </Center>
      }
    />
  )
}
