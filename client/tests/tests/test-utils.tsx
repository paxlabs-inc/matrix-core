import type { ReactElement, ReactNode } from 'react'
import { render, type RenderOptions } from '@testing-library/react'
import { NextIntlClientProvider } from 'next-intl'
import messages from '@/messages/en.json'

/** Wrap a tree in the same i18n provider the app uses, with the real
 *  English message catalogue, so component tests exercise the actual
 *  translated copy rather than placeholder keys. */
export function IntlWrapper({ children }: { children: ReactNode }) {
  return (
    <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
      {children}
    </NextIntlClientProvider>
  )
}

export function renderWithIntl(ui: ReactElement, options?: Omit<RenderOptions, 'wrapper'>) {
  return render(ui, { wrapper: IntlWrapper, ...options })
}

export * from '@testing-library/react'
