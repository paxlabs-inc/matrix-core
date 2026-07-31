import { Suspense } from 'react'
import Image from 'next/image'
import { Layout, Section, VStack } from '@astryxdesign/core/Layout'
import { Heading, Text } from '@astryxdesign/core/Text'
import LoginForm from './login-form'
import { MatrixLogo } from '@/components/matrix/matrix-logo'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata = {
  title: 'Sign in',
}

export default function LoginPage() {
  return (
    <Layout
      height="auto"
      padding={0}
      className="bg-background min-h-screen"
      content={
        <main className="grid min-h-screen grid-cols-1 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
          <Section
            variant="transparent"
            padding={0}
            className="relative flex min-h-screen flex-col px-6 py-8 sm:px-10 lg:px-16"
          >
            <MatrixLogo size="md" />

            <VStack gap={8} className="mx-auto w-full max-w-sm flex-1 justify-center py-10">
              <VStack gap={3}>
                <Heading
                  level={1}
                  type="display-1"
                  style={{ fontSize: 'clamp(2.5rem, 5vw, 4rem)', lineHeight: 0.98 }}
                >
                  Think fast,
                  <br />
                  build faster
                </Heading>
              </VStack>

              <Suspense fallback={null}>
                <LoginForm />
              </Suspense>
            </VStack>

            <Text type="supporting" color="secondary">
              &copy; {new Date().getFullYear()} Matrix
            </Text>
          </Section>

          <Section variant="muted" padding={0} className="m-3 hidden overflow-hidden lg:block">
            <div className="relative h-full min-h-[calc(100vh-1.5rem)] w-full overflow-hidden rounded-3xl">
              <Image
                src="/Welcome_s.png"
                alt=""
                fill
                priority
                sizes="50vw"
                className="object-cover"
              />
            </div>
          </Section>
        </main>
      }
    />
  )
}
