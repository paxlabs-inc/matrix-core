# pkg/lxp — the LXP (lxp/1) paywall kit

HTTP-native payments on LayerX for any Go service: a priced request answers
`402` with lxp/1 terms (a prefetched single-use LayerX nonce + USDX amount),
the payer retries with a signed `X-LayerX-Payment`, the middleware settles
through layerxd and responds with `X-LayerX-Receipt`. The service never
custodies funds and never signs on a payer's behalf; layerxd unreachable is
`503 payment_unavailable`, never a free call. Protocol spec:
`docs/08-payments-billing.md`.

## Go (this package)

```go
srv, err := lxp.New(lxp.Config{
    LayerXURL: os.Getenv("LAYERX_URL"),
    KeyHex:    os.Getenv("SERVICE_KEY"), // ed25519 seed; required for hold mode
    DIDLabel:  "my-service",
})
if err != nil {
    log.Fatal(err)
}

price := func(r *http.Request) (lxp.Price, bool, error) {
    if r.URL.Path != "/premium" {
        return lxp.Price{}, false, nil // free route
    }
    return lxp.Price{
        AmountUSDX: "0.010000",
        PayTo:      "did:matrix:me:0011223344556677",
        Mode:       lxp.ModeHold, // or lxp.ModeExact (default)
        TTLSeconds: 60,
    }, true, nil
}

http.Handle("/", srv.Middleware(price)(myHandler))
```

Exact mode settles then serves; hold mode reserves in the payer's own
account, serves buffered, captures on 2xx and releases on anything else.
The deus gateway consumes this same package — one implementation, dogfooded.

## Node (`runner/src/lxp.js`)

```js
import { createLXP } from './lxp.js'

const lxp = createLXP({ layerxUrl, keyHex, didLabel: 'my-service' })
const guarded = lxp.guard(
  (req) => (req.url === '/premium'
    ? { amount_usdx: '0.010000', pay_to: 'did:matrix:me:0011223344556677', mode: 'hold', ttl_s: 60 }
    : null),
  handler,
)
http.createServer((req, res) => guarded(req, res)).listen(8080)
```

The runner harness enables this for `/invoke` when `LXP_LAYERX_URL`,
`LXP_PRICE_USDX`, and `LXP_PAY_TO` are set (`LXP_KEY` for hold mode).

## Client half

`tools/deus/deus.mjs` handles 402s automatically under the owner leash
(`LAYERX_MAX_SPEND_USDX` per call, `LAYERX_MAX_DAILY_USDX` rolling daily) and
surfaces the receipt; `EncodePayment`/`DecodeReceipt` in this package are the
Go client half. Preimage lockstep across Go/Node/client is pinned by
`crossimpl_test.go` and `nodemw_test.go`.
