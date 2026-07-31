/**
 * Optimistic portfolio adjustments for fund/sweep mutations.
 * Uses BigInt for balance_raw — never float math on token amounts.
 */
import type { WalletPortfolio } from '@/lib/paxeer/explorer'
import { formatBalance } from '@/lib/paxeer/format'

function addWei(a: string, b: string): string {
  return (BigInt(a || '0') + BigInt(b || '0')).toString()
}

function subtractWei(a: string, b: string): string {
  const diff = BigInt(a || '0') - BigInt(b || '0')
  return diff < BigInt(0) ? '0' : diff.toString()
}

function adjustHolding(
  portfolio: WalletPortfolio,
  amountWei: string,
  tokenAddress: string | undefined,
  direction: 'debit' | 'credit',
): WalletPortfolio {
  const op = direction === 'debit' ? subtractWei : addWei
  const next: WalletPortfolio = {
    ...portfolio,
    token_holdings: portfolio.token_holdings.map((h) => ({ ...h })),
    native_balance: { ...portfolio.native_balance },
    computed_at: new Date().toISOString(),
  }

  if (!tokenAddress) {
    next.native_balance.balance_raw = op(next.native_balance.balance_raw, amountWei)
    next.native_balance.balance = formatBalance(next.native_balance.balance_raw, 18, 6)
    return next
  }

  const addr = tokenAddress.toLowerCase()
  const idx = next.token_holdings.findIndex((h) => h.contract_address.toLowerCase() === addr)
  if (idx >= 0) {
    const h = next.token_holdings[idx]
    h.balance_raw = op(h.balance_raw, amountWei)
    h.balance = formatBalance(h.balance_raw, h.decimals, 6)
  }

  return next
}

/** Apply a transfer optimistically to one portfolio address cache entry. */
export function optimisticPortfolioTransfer(
  portfolio: WalletPortfolio,
  amountWei: string,
  tokenAddress: string | undefined,
  direction: 'debit' | 'credit',
): WalletPortfolio {
  return adjustHolding(portfolio, amountWei, tokenAddress, direction)
}
