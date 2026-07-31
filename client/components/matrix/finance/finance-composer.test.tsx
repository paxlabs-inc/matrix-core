import { StrictMode, type ReactNode } from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { NextIntlClientProvider } from 'next-intl'
import {
  AppRouterContext,
  type AppRouterInstance,
} from 'next/dist/shared/lib/app-router-context.shared-runtime'
import { SearchParamsContext } from 'next/dist/shared/lib/hooks-client-context.shared-runtime'
import { describe, expect, it } from 'vitest'

import {
  askHref,
  FinanceComposer,
  useHandoffAsk,
} from '@/components/matrix/finance/finance-composer'
import messages from '@/messages/en.json'

function routerRecorder() {
  const pushes: string[] = []
  const replacements: string[] = []
  const router: AppRouterInstance = {
    back: () => window.history.back(),
    forward: () => window.history.forward(),
    refresh: () => window.dispatchEvent(new Event('popstate')),
    push: (href) => {
      pushes.push(href)
      window.history.pushState(null, '', href)
    },
    replace: (href) => {
      replacements.push(href)
      window.history.replaceState(null, '', href)
    },
    prefetch: () => undefined,
  }
  return { router, pushes, replacements }
}

function Handoff({ send }: { send: (text: string) => void }) {
  useHandoffAsk(send)
  return null
}

function navigationWrapper(router: AppRouterInstance, params: URLSearchParams, strict = false) {
  return function NavigationWrapper({ children }: { children: ReactNode }) {
    const tree = (
      <NextIntlClientProvider locale="en" messages={messages}>
        <AppRouterContext.Provider value={router}>
          <SearchParamsContext.Provider value={params}>{children}</SearchParamsContext.Provider>
        </AppRouterContext.Provider>
      </NextIntlClientProvider>
    )
    return strict ? <StrictMode>{tree}</StrictMode> : tree
  }
}

describe('finance composer handoff', () => {
  it('navigates to the ordinary conversation route with one encoded question and context', async () => {
    const user = userEvent.setup()
    const navigation = routerRecorder()
    render(<FinanceComposer context="AAPL — Apple Inc." placeholder="Ask Neo about AAPL" />, {
      wrapper: navigationWrapper(navigation.router, new URLSearchParams()),
    })

    await user.type(screen.getByRole('textbox', { name: 'Ask Neo about AAPL' }), 'Why is it down?')
    await user.keyboard('{Enter}')

    expect(navigation.pushes).toHaveLength(1)
    const handoff = new URL(navigation.pushes[0], 'https://matrix.test')
    expect(handoff.pathname).toBe('/')
    expect(handoff.searchParams.get('ask')).toBe('Why is it down?')
    expect(handoff.searchParams.get('ask_context')).toBe('AAPL — Apple Inc.')
    expect(screen.getByRole('textbox', { name: 'Ask Neo about AAPL' })).toHaveTextContent('')
  })

  it('sends exactly one ordinary turn, carries context, and clears the handoff parameters', async () => {
    const navigation = routerRecorder()
    const turns: string[] = []
    const params = new URLSearchParams({
      ask: 'Why is it down?',
      ask_context: 'AAPL — Apple Inc.',
    })
    const send = (text: string) => turns.push(text)
    const view = render(<Handoff send={send} />, {
      wrapper: navigationWrapper(navigation.router, params, true),
    })

    await waitFor(() => expect(turns).toHaveLength(1))
    expect(turns).toEqual([
      "Why is it down?\n\n(I'm looking at AAPL — Apple Inc. on the markets page.)",
    ])
    expect(navigation.replacements).toEqual(['/'])

    view.rerender(<Handoff send={send} />)
    expect(turns).toHaveLength(1)
    expect(navigation.replacements).toHaveLength(1)
  })

  it('does nothing without a question and never invents a finance chat endpoint', async () => {
    const navigation = routerRecorder()
    const turns: string[] = []
    render(<Handoff send={(text) => turns.push(text)} />, {
      wrapper: navigationWrapper(
        navigation.router,
        new URLSearchParams({ ask_context: 'Markets' }),
      ),
    })

    await Promise.resolve()
    expect(turns).toEqual([])
    expect(navigation.replacements).toEqual([])
    expect(askHref('Market recap')).toBe('/?ask=Market+recap')
  })
})
