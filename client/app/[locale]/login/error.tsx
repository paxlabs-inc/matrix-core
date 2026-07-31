'use client'

import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { EmptyState } from '@astryxdesign/core/EmptyState'
import { Button } from '@astryxdesign/core/Button'

export default function LoginError({ reset }: { reset: () => void }) {
  return (
    <Layout
      height="fill"
      padding={6}
      className="bg-background min-h-screen"
      content={
        <Center>
          <EmptyState
            headingLevel={1}
            title="Sign-in unavailable"
            description="Something went wrong loading the sign-in page. Try again or return home."
            actions={
              <>
                <Button label="Home" href="/" variant="secondary" />
                <Button label="Try again" onClick={reset} variant="primary" />
              </>
            }
          />
        </Center>
      }
    />
  )
}
