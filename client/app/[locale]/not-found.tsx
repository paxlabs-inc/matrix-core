import { getTranslations } from 'next-intl/server'
import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { EmptyState } from '@astryxdesign/core/EmptyState'
import { Button } from '@astryxdesign/core/Button'
import { Text } from '@astryxdesign/core/Text'
import { CentraLogo } from '@/components/brand/centra-logo'

export default async function NotFound() {
  const t = await getTranslations('notFound')

  return (
    <Layout
      height="fill"
      padding={6}
      className="bg-background min-h-screen"
      content={
        <Center>
          <EmptyState
            headingLevel={1}
            title={t('title')}
            description={t('message')}
            icon={
              <div className="flex flex-col items-center gap-4">
                <CentraLogo size="lg" />
                <Text type="display-1" color="accent" hasTabularNumbers>
                  {t('code')}
                </Text>
              </div>
            }
            actions={<Button label={t('back')} href="/" variant="primary" />}
          />
        </Center>
      }
    />
  )
}
