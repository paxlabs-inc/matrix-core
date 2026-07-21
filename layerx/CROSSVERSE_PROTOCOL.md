# Crossverse protocol — AUTHORITATIVE (owner-provided 2026-07-20)

Crossverse is NOT one system: it is 18 per-symbol Symbol Services + a Markets
Service + a Chain Explorer Service behind one load balancer. NOTHING in
Crossverse will change — LayerX adapts to this contract exactly.

## WebSocket protocol (shared by all services)

Every service exposes its WebSocket on the same host:port as its HTTP API.
Connect with a plain WebSocket — no auth, no subprotocol, path `/`.

### Client → Server frames

```
{ "event": "subscribe",   "topic": "<topic>" }
{ "event": "unsubscribe", "topic": "<topic>" }
{ "event": "ping" }
```

Some snapshot providers accept additional fields on subscribe. The only one
currently honored is `limit` on kline topics.

### Server → Client control frames

```
{ "event": "welcome",      "ts": 1747526400000, "snapshotId": "lqz3a7-9f2b1c0d4e5f" }
{ "event": "subscribed",   "topic": "BTC@orderbook", "ts": ..., "snapshotId": "..." }
{ "event": "unsubscribed", "topic": "BTC@orderbook", "ts": ... }
{ "event": "pong",         "ts": ... }
{ "event": "error",        "message": "<code>", "ts": ..., "topic"?: "..." }
```

`error.message` values: `invalid_json`, `missing_topic`, `unknown_event`,
`subscribe_rate_limit`, `too_many_subscriptions`.

### Data frames

Every data frame on every topic has this envelope:

```
{
  "topic":      "<topic>",
  "ts":         <ms since epoch>,
  "snapshotId": "<process id>",
  "seq":        <integer>,
  "snapshot":   true | undefined,
  "data":       { ... topic-specific payload ... }
}
```

### Stream consistency contract

- `snapshotId` — opaque string, constant for the lifetime of the server
  process. If it changes mid-stream, the server has restarted. Drop local
  state and resubscribe.
- `seq` — monotonically increasing integer per topic, dense (every delta
  increments by exactly 1).
- `snapshot: true` is sent once per subscribe, immediately after the
  `subscribed` ack. It carries the seq of the last broadcast at
  snapshot-build time. Apply the snapshot, then accept deltas with
  `seq > snapshot.seq`.
- Gap detection — if `delta.seq > lastSeq + 1`, frames were dropped. Treat
  local state as stale: fetch the matching REST endpoint to resync, then
  resume with deltas where `seq > restSnapshot.seq` once you re-subscribe.

### Connection lifetime

- Server sends WebSocket pings every 30s; respond with pongs. Dead sockets
  are dropped on the next interval.
- Per-socket caps: max 200 subscriptions, max 50 subscribe ops/second.

## Symbol Service

One instance per tradable symbol (BTC, ETH, SOL, …). Each instance owns one
symbol — the `<SYMBOL>` token in every topic and path is fixed per instance.

Base URL: `http(s)://<symbol-host>:<symbol-port>`
WebSocket: `ws(s)://<symbol-host>:<symbol-port>`

### REST — market data

`GET /price`
```
{ "symbol": "BTC", "price": 77173.0, "timestamp": "2026-05-18T00:00:00.000Z",
  "open": 76800.0, "high": 77310.0, "low": 76590.0 }
```

`GET /stats` — rolling 24h spot statistics.
```
{ "symbol": "BTC", "volume_24h": 12345678.9, "open_interest": 98765432.1,
  "est_funding_rate": 0.00012, "last_funding_rate": 0.00009 }
```

`GET /perp_stats` — same shape as `/stats` plus perp-only fields.
```
{ "symbol": "BTC", "volume_24h": ..., "open_interest": ...,
  "est_funding_rate": ..., "last_funding_rate": ...,
  "mark_price": 77182.4, "index_price": 77173.0, "basis_bps": 1.21,
  "long_short_ratio": 1.03, "liq_volume_24h": ... }
```

`GET /trades/:symbol?limit=100` — recent spot trades, newest first, limit
clamped [1,1000] default 100:
```
{ "symbol": "BTC", "count": 100, "trades": [
  { "id": "1747526400000-0", "side": "BUY", "price": 77175, "size": 0.42, "trade_time": 1747526400123 } ] }
```

`GET /perp_trades/:symbol?limit=100` — recent perp trades, newest first:
```
{ "symbol": "BTC", "count": 100, "trades": [
  { "id": "1747526400000-0", "side": "BUY", "price": 77182.4, "contracts": 25,
    "notional": 250, "liquidation": false, "trade_time": 1747526400123 } ] }
```

`GET /health`
```
{ "status": "ok", "backfill": "complete", "lastCandle": "2026-05-18T00:00:00.000Z", "symbol": "BTC" }
```

### REST — TradingView UDF datafeed

`GET /config`, `GET /time`, `GET /symbols?symbol=`, `GET /search?query=&limit=`,
`GET /history?symbol=&resolution=&from=&to=&countback=` (resolutions
1,2,3,5,15,30,60,240,480,D,W), `GET /quotes?symbols=`.

### WebSocket topics (per symbol instance)

`<SYMBOL>@orderbook` — full L2 book every tick; asks ascending, bids
descending; up to 100 levels/side; each level `[price, size]`.

`<SYMBOL>@trade` — one frame per spot trade
(`{symbol, side, price, size, trade_time}`); subscribe snapshot carries the
most recent 100 trades newest-first in `data.trades`.

`<SYMBOL>@stats` — rolling stats, broadcast every 30s and on subscribe
(`{symbol, volume_24h, open_interest, est_funding_rate, last_funding_rate}`).

`<SYMBOL>@kline_<interval>` — intervals 1m,2m,3m,5m,15m,30m,1h,4h,8h,1D,1W;
subscribe may include `"limit": N` (default 1000, max 5000). Snapshot
`data.candles: [[t,o,h,l,c,v,closed],...]`; live `data.candle: [t,...]`.
`t` = bucket start seconds; `closed` 1 final / 0 open. On rollover: previous
candle with closed:1, then new candle with closed:0. Same t ⇒ update; new t
⇒ new bar.

`<SYMBOL>@perp_orderbook` — perp L2 book, same shape as spot PLUS
mark/index/basis metadata IN the frame:
```
"data": { "symbol": "BTC", "mark_price": 77182.4, "index_price": 77173.0,
          "basis_bps": 1.21, "asks": [[77185, 25], ...], "bids": [[77180, 40], ...] }
```
Sizes are in contracts (integers), not base units.

`<SYMBOL>@perp_trade` — one frame per perp trade
(`{symbol, side, price, contracts, notional, liquidation, trade_time}`);
subscribe snapshot mirrors `@trade` (recent 100, newest first).

`<SYMBOL>@perp_stats` — broadcast every 30s and on subscribe:
```
"data": { "symbol": "BTC", "volume_24h": ..., "open_interest": ...,
          "est_funding_rate": ..., "last_funding_rate": ...,
          "mark_price": 77182.4, "index_price": 77173.0, "basis_bps": 1.21,
          "long_short_ratio": 1.03, "liq_volume_24h": ... }
```

## Markets Service

Single instance. Cross-symbol market table and platform totals.
Base URL: `http(s)://<markets-host>:3020`; WS same host:port.

### REST

`GET /markets` — all markets:
```
{ "ts": 1747526400123, "markets": [
  { "symbol": "BTC", "symbol_full": "Bitcoin", "price": 77173.0,
    "change_24h": 0.0123, "volume_24h": 1234567.8, "open_interest": 987654.3,
    "funding_8h": 0.00009, "est_funding_rate": 0.00012, "volume_7d": ...,
    "volume_30d": ..., "trades_count": 123456, "last_updated": 1747526399000,
    "perp_mark_price": 77182.4, "perp_index_price": 77173.0,
    "perp_basis_bps": 1.21, "perp_funding_rate": 0.00009,
    "perp_next_funding_ms": 1747555200000, "perp_long_short_ratio": 1.03,
    "perp_liq_volume_24h": ..., "perp_oi_contracts": ..., "perp_oi_usd": ...,
    "perp_volume_24h_contracts": ..., "perp_volume_24h_usd": ... }, ... ] }
```

`GET /markets/:symbol` — `{ "ts": ..., "market": { ...one record... } }`;
404 `{ "error": "Unknown symbol: ..." }`.

`GET /totals` —
```
{ "ts": ..., "totals": { "total_trades": 1234567, "volume_24h": ...,
  "volume_7d": ..., "volume_30d": ..., "total_oi": ... } }
```

`GET /health` —
`{ "ok": true, "service": "markets-aggregator", "symbols": 18, "uptimeSec": 12345 }`

### WebSocket topics

`markets@all` — full array of all market records (same shape as REST
`markets`), broadcast every 5s and on subscribe.
`markets@totals` — totals object, broadcast every 5s and on subscribe.
`<SYMBOL>@market` — single-market record, broadcast every 5s while
subscribed.

## Chain Explorer Service

Single instance. Base URL: `http(s)://<chain-host>:4500`; WS same host:port.

`GET /blocks?limit=N` (clamp [1,50] default 20) — recent blocks
`{block_number, time, proposer, hash, transactions}` newest first.
`GET /transactions?limit=N` (clamp [1,200] default 50) — recent txs
`{hash, action, block, time, user, symbol, side, price, size}`; `action` one
of "Place Order", "Cancel Order", "Modify Order", "OROB Fill",
"Oracle Updated", "Sealed Batch Executed".
`GET /health` — `{status, ts, chain_connected, blocks_seen, txs_seen}`.

WS: `chain@blocks` (live one-per-block; subscribe snapshot = array of recent
20) and `chain@transactions` (live one-per-tx; snapshot = recent 50). Both use
the standard envelope with `snapshot: true` on the snapshot frame.
