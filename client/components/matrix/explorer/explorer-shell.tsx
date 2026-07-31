'use client'

/**
 * LayerX explorer shell — header, tab navigation and the universal search
 * box (seq, DID, batch id, batch root). Wraps every /explorer page.
 */
import { useState, type FormEvent, type ReactNode } from 'react'
import { useTranslations } from 'next-intl'
import { Layout, LayoutContent, LayoutHeader, VStack } from '@astryxdesign/core/Layout'
import { Heading, Text } from '@astryxdesign/core/Text'
import { TextInput } from '@astryxdesign/core/TextInput'
import { Tab, TabList } from '@astryxdesign/core/TabList'
import { Button } from '@astryxdesign/core/Button'
import { usePathname, useRouter } from '@/i18n/navigation'
import { Search } from '@/lib/matrix-icons'
import { getLayerXAnchor } from '@/lib/layerx/explorer'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const ROOT_RE = /^(0x)?[0-9a-f]{64}$/i

export function ExplorerShell({ children }: { children: ReactNode }) {
  const t = useTranslations('layerxExplorer')
  const pathname = usePathname()
  const router = useRouter()
  const [query, setQuery] = useState('')
  const [notFound, setNotFound] = useState(false)
  const [searching, setSearching] = useState(false)

  const tabs = [
    { href: '/explorer', label: t('tabOverview') },
    { href: '/explorer/transfers', label: t('tabTransfers') },
    { href: '/explorer/batches', label: t('tabBatches') },
  ] as const

  async function onSearch(e: FormEvent) {
    e.preventDefault()
    const q = query.trim()
    if (!q || searching) return
    setNotFound(false)
    if (/^\d+$/.test(q)) {
      router.push(`/explorer/tx/${q}`)
      return
    }
    if (q.startsWith('did:')) {
      router.push(`/explorer/account/${encodeURIComponent(q)}`)
      return
    }
    if (UUID_RE.test(q)) {
      router.push(`/explorer/batch/${q}`)
      return
    }
    if (ROOT_RE.test(q)) {
      setSearching(true)
      try {
        const anchor = await getLayerXAnchor(q)
        router.push(`/explorer/batch/${anchor.batch_id}`)
      } catch {
        setNotFound(true)
      } finally {
        setSearching(false)
      }
      return
    }
    setNotFound(true)
  }

  const activeTab =
    tabs.find((tab) =>
      tab.href === '/explorer' ? pathname === '/explorer' : pathname.startsWith(tab.href),
    )?.href ?? '/explorer'

  return (
    <Layout
      height="auto"
      contentWidth={960}
      padding={4}
      className="bg-background min-h-dvh"
      header={
        <LayoutHeader>
          <VStack gap={4} width="100%">
            <div className="flex flex-wrap items-end justify-between gap-3">
              <VStack gap={1}>
                <Heading level={1} type="display-3">
                  {t('title')}
                </Heading>
                <Text type="supporting" color="secondary">
                  {t('subtitle')}
                </Text>
              </VStack>
              <Button label={t('backToApp')} href="/" variant="secondary" size="sm" />
            </div>

            <form onSubmit={onSearch}>
              <TextInput
                label={t('searchPlaceholder')}
                isLabelHidden
                value={query}
                onChange={(value) => {
                  setQuery(value)
                  setNotFound(false)
                }}
                placeholder={t('searchPlaceholder')}
                startIcon={<Search className="size-4" />}
                isLoading={searching}
                status={notFound ? { type: 'error', message: t('searchNotFound') } : undefined}
                width="100%"
              />
            </form>

            <TabList
              value={activeTab}
              onChange={(value) => router.push(value as '/explorer')}
              size="md"
              aria-label={t('title')}
            >
              {tabs.map((tab) => (
                <Tab key={tab.href} value={tab.href} href={tab.href} label={tab.label} />
              ))}
            </TabList>
          </VStack>
        </LayoutHeader>
      }
      content={
        <LayoutContent>
          <VStack gap={4} width="100%">
            {children}
          </VStack>
        </LayoutContent>
      }
    />
  )
}
