-- deus 006_usdx_pricing — USDX-native pricing (DEUS-LAYERX req.7).
--
-- Pricing plans and quotes gain USDX (6dp decimal) denominations alongside the
-- legacy PAX wei columns, which lose NOT NULL so USDX-only plans can land. The
-- wei columns themselves die with the rail deletion (task 6.1).
--
-- Existing wei rows are converted at 1 PAX = 1 USDX (1e18 wei = 1e6 micro-USDX,
-- i.e. micro = ceil(wei / 1e12), floored at 1 micro for positive prices so no
-- priced row becomes free). The UPDATEs are guarded on IS NULL, so re-running
-- the statements is a no-op. Forward-only.

BEGIN;

ALTER TABLE pricing_plans ADD COLUMN IF NOT EXISTS price_usdx TEXT;
ALTER TABLE pricing_plans ADD COLUMN IF NOT EXISTS min_charge_usdx TEXT;
ALTER TABLE pricing_plans ALTER COLUMN price_wei DROP NOT NULL;
ALTER TABLE pricing_plans ALTER COLUMN min_charge_wei DROP NOT NULL;

UPDATE pricing_plans SET
    price_usdx = (GREATEST(CEIL(price_wei::numeric / 1000000000000),
                           CASE WHEN price_wei::numeric > 0 THEN 1 ELSE 0 END)
                  / 1000000.0)::numeric(20,6)::text,
    min_charge_usdx = (GREATEST(CEIL(min_charge_wei::numeric / 1000000000000),
                                CASE WHEN min_charge_wei::numeric > 0 THEN 1 ELSE 0 END)
                       / 1000000.0)::numeric(20,6)::text
WHERE price_usdx IS NULL AND price_wei IS NOT NULL;

ALTER TABLE quotes ADD COLUMN IF NOT EXISTS unit_price_usdx TEXT;
ALTER TABLE quotes ALTER COLUMN unit_price_wei DROP NOT NULL;

UPDATE quotes SET
    unit_price_usdx = (GREATEST(CEIL(unit_price_wei::numeric / 1000000000000),
                                CASE WHEN unit_price_wei::numeric > 0 THEN 1 ELSE 0 END)
                       / 1000000.0)::numeric(20,6)::text
WHERE unit_price_usdx IS NULL AND unit_price_wei IS NOT NULL;

COMMIT;
