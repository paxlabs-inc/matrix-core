# ADR 002: Stale data tolerances (real-time)

## Status

Accepted — 2026-06-06

## Policy

| Data                    | Max staleness    | Transport           | On stale                          |
| ----------------------- | ---------------- | ------------------- | --------------------------------- |
| Run status / transcript | 0 (live)         | SSE + REST snapshot | Gap → resync via replay           |
| Chat narration          | 0 (live)         | SSE                 | Same connection hub               |
| Wallet balances         | 0 after mutation | REST                | `invalidateQueries` on fund/sweep |
| Receipts / history      | 30s              | REST                | Background refetch OK             |
| Settings / policies     | 30s              | REST                | User-triggered save invalidates   |

Prices and balances shown during fund/sweep use `BigInt`/`parseUnits` — never
float settlement math.
