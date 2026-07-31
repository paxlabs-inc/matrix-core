'use client'

import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { EmptyState } from '@astryxdesign/core/EmptyState'
import { Button } from '@astryxdesign/core/Button'

export default function AuthCallbackError({ reset }: { reset: () => void }) {
  return (
    <Layout
      height="fill"
      padding={6}
      className="bg-background min-h-dvh"
      content={
        <Center>
          <EmptyState
            headingLevel={1}
            title="Sign-in failed"
            description="We could not complete authentication. Your link may have expired."
            actions={
              <>
                <Button label="Back to sign in" href="/login" variant="secondary" />
                <Button label="Try again" onClick={reset} variant="primary" />
              </>
            }
          />
        </Center>
      }
    />
  )
}
