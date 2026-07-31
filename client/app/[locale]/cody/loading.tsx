import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { Spinner } from '@astryxdesign/core/Spinner'

export default function Loading() {
  return (
    <Layout
      height="fill"
      padding={6}
      className="bg-background h-svh"
      content={
        <Center>
          <Spinner size="xl" label="Loading Neo…" />
        </Center>
      }
    />
  )
}
