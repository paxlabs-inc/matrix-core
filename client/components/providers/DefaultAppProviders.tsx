'use client'

import { type ReactNode } from 'react'
import Link from 'next/link'
import { MotionConfig } from 'motion/react'
import { Theme } from '@astryxdesign/core/theme'
import { LinkProvider } from '@astryxdesign/core/Link'
import { LayerProvider } from '@astryxdesign/core/Layer'
import { InternationalizationProvider } from '@astryxdesign/core/i18n'
import { DesignModeProvider } from '@/components/design'
import { AuthProvider } from '@/lib/auth/AuthProvider'
import { QueryProvider } from '@/lib/query/QueryProvider'
import { matrixTheme } from '@/lib/astryx/matrix-theme'

export function DefaultAppProviders({ children, locale }: { children: ReactNode; locale: string }) {
  return (
    <Theme theme={matrixTheme} mode="dark">
      <InternationalizationProvider locale={locale}>
        <LinkProvider component={Link}>
          <LayerProvider>
            <MotionConfig reducedMotion="user">
              <QueryProvider>
                <AuthProvider>
                  <DesignModeProvider>{children}</DesignModeProvider>
                </AuthProvider>
              </QueryProvider>
            </MotionConfig>
          </LayerProvider>
        </LinkProvider>
      </InternationalizationProvider>
    </Theme>
  )
}
