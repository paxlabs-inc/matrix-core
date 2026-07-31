import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { Spinner } from '@astryxdesign/core/Spinner'

export default function AuthCallbackLoading() {
  return (
    <Layout
      height="fill"
      padding={6}
      className="bg-background min-h-dvh"
      content={
        <Center>
          <Spinner size="xl" label="Signing in" />
        </Center>
      }
    />
  )
}
