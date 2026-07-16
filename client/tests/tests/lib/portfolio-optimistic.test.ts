import { describe, it, expect } from 'vitest'
import { optimisticPortfolioTransfer } from '@/lib/paxeer/portfolio-optimistic'
import type { WalletPortfolio } from '@/lib/paxeer/explorer'

const base: WalletPortfolio = {
  address: '0xabc',
  native_balance: {
    symbol: 'PAX',
    balance_raw: '1000000000000000000',
    balance: '1.0',
    price_usd: '1',
    value_usd: '1',
  },
  token_holdings: [],
  total_value_usd: '1',
  token_count: 0,
  transaction_count: 0,
  transfer_count: 0,
  computed_at: '2026-01-01T00:00:00Z',
}

describe('optimisticPortfolioTransfer', () => {
  it('debits native balance', () => {
    const next = optimisticPortfolioTransfer(base, '500000000000000000', undefined, 'debit')
    expect(next.native_balance.balance_raw).toBe('500000000000000000')
  })

  it('credits native balance', () => {
    const next = optimisticPortfolioTransfer(base, '1000000000000000000', undefined, 'credit')
    expect(next.native_balance.balance_raw).toBe('2000000000000000000')
  })

  it('does not go negative on debit', () => {
    const next = optimisticPortfolioTransfer(base, '9000000000000000000', undefined, 'debit')
    expect(next.native_balance.balance_raw).toBe('0')
  })
})
