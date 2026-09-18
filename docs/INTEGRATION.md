# Freedom ⇄ Tapp integration

*The card side is Tapp (`/Users/mac/tapp-platform`, Go, Railway). The market side
is Freedom (this repo). This document is the contract between them. Written
2026-09-18; both sides are built against it.*

## The shape

Tapp is already a working card rail: a cardholder taps at a merchant registered
in the Tapp merchant app, the cardholder's ledger is debited, the merchant's
`merchant_payable` is credited with `amount − fee`, the fee (0.5%, `CARD_FEE_BPS`)
lands in Tapp's `revenue`, and the naira reaches the merchant's bank later via
Paycrest. Freedom is already a working market: a fee split → a buyback intent →
an allocation from the company's treasury at the auction price → a holding lot
→ an auction in which the holder can sell.

What is missing is a **door** between them. Not a rewrite of either.

```
tapp-platform                                   freedom (exchanged)
─────────────                                   ───────────────────
merchant registers business ──POST /v1/rail/businesses──▶ company + listing
                                                          application, assessed
                                                          against the rulebook,
                                                          admitted, instrument
                                                          + treasury created
tap charged (ledger)  ──outbox──POST /v1/rail/taps──────▶ presentment → clearing
                                                          → fee split → buyback
                                                          intent → allocation
                                                          (if today's session
                                                          has a price) → lot
cardholder opens app  ──GET /v1/rail/cardholders/{ref}/holdings──▶ lots, value
```

Freedom's ledger treats Tapp as one **scheme participant** that is both the
issuer and the acquirer of every tap it delivers. That is not a shortcut, it is
the accounting: Tapp collected the ticket from the cardholder and paid the
merchant itself, so after clearing its net position with the scheme is exactly
the merchant service charge it owes. Every leg is a normal ledger posting:

| moment | posting (NGN) | reason |
|---|---|---|
| ingest | participant `settlement` −ticket, cardholder `hold` +ticket | `rail.tap_funded` — the hold an authorisation would have placed, funded by the participant who already collected it |
| clearing (unchanged) | hold −ticket, merchant_receivable +(ticket−MSC), interchange, revenue, buyback_pool | `clearing.*` |
| after clearing | merchant_receivable −(ticket−MSC), participant `settlement` +(ticket−MSC) | `rail.merchant_paid` — the acquirer already paid the merchant |

Net: participant settlement = −MSC. Buyback pool = 10% of MSC (+ any merchant
co-funding). Interchange income = 60% of MSC, to the same participant.

## Freedom: the rail API

Served by `exchanged` under `/v1/rail/*`. Service-to-service: every call carries
`Authorization: Bearer $RAIL_TOKEN` (env, required; the server refuses to start
with `/v1/rail` mounted if unset). No per-member scoping — the caller is the
rail. All money is kobo (`int64`), all quantities are `share.Units` (1e‑8 of a
share) serialised as `int64` plus a human `"shares"` string where shown.

`external_ref` values are Tapp's own ids (sender profile id for merchants, user
id for cardholders, tap id for taps). Freedom stores them and never interprets
them.

### Businesses (onboarding = listing)

`POST /v1/rail/businesses`
```json
{
  "merchant_ref": "sp_…",               "cardholder_ref": "usr_… (the owner, optional)",
  "legal_name": "Mama Put Kitchens Ltd", "trading_name": "Mama Put",
  "rc_number": "RC1483920",             "mcc": "5812",
  "symbol": "MAMAPUT",
  "evidence": {
    "trading_months": 30, "audited_accounts": true, "auditor_on_list": true,
    "shares_in_issue": 800000000000000,   "public_shares": 120000000000000,
    "holders": 31, "treasury_units": 180000000000000,
    "board_resolution": true, "directors_clear": true,
    "net_assets_kobo": 8000000000, "revenue_kobo": 24000000000
  },
  "shares_authorised_units": 1000000000000000,
  "daily_release_units": 50000000000000,
  "cofund_bps": 0,
  "holders": [ {"cardholder_ref": "usr_…", "units": 50000000000000, "label": "founder"} ]
}
```
Behaviour, in one transaction: create `companies` + `merchants` (with
`external_ref`, `company_id`, `cofund_bps`) if absent → `exchange.Apply` →
`exchange.Assess(StandardCriteria(), evidence)` → `exchange.ListingPrice`
sets the price from the audited figures (LISTING-RULES §2.4) and records it as
the `listing_price` finding → if every finding is met,
`exchange.AdmitListing(price, authorised, dailyRelease, by="rail")` → then
distribute `holders` from treasury as zero-cost lots dated today, unlocked
(`transferable_from` = listing date; these are founders' shares, not tap
earnings). Response `201`:
```json
{ "symbol": "MAMAPUT", "instrument_id": "…", "state": "listed",
  "findings": [{"criterion":"free_float","met":true,"detail":"15.0% ≥ 10%"}, …,
               {"criterion":"listing_price","met":true,
                "detail":"fair value ₦320,000,000.00 (net assets ₦80,000,000.00 + 1.0× revenue ₦240,000,000.00) over 8000000 shares → listed at ₦40.00"}],
  "reference_price_kobo": 4000, "fair_value_kobo": 32000000000,
  "treasury_units": 130000000000000 }
```
If a finding is unmet the application is recorded and returned with `state:
"rejected"` (HTTP `200`, not an error — the merchant needs the findings). A
second call for the same `merchant_ref` returns the existing record (`200`).

`GET /v1/rail/businesses/{merchant_ref}` — the record above plus the cap table:
`shares_authorised`, `in_issue`, `treasury_remaining`, `released_today`,
`daily_release_units`, `holders` (count), `top_holders` (up to 10, by units,
with `cardholder_ref`), `pending_funding_kobo` (intents not yet allocated),
`escrowed_funding_kobo`, `reference_price_kobo`, `last_session` (date, state,
price, volume), `halted` (bool + reason).

`GET /v1/rail/businesses/{merchant_ref}/holders?limit=&cursor=` — every holder:
`cardholder_ref`, `units`, `cost_kobo`, `first_acquired`, `locked_units`.

### Taps

`POST /v1/rail/taps`
```json
{ "tap_ref": "tap_…", "merchant_ref": "sp_…", "cardholder_ref": "usr_…",
  "cardholder_display_name": "Ada", "amount_kobo": 1000000,
  "charged_at": "2026-09-18T11:24:03Z" }
```
Behaviour, one transaction, idempotent on `tap_ref`: upsert cardholder (by
`external_ref`; creates the one synthetic `cards` row that `presentments`
needs — product_code `tapp`, issuer = the Tapp participant) → post
`rail.tap_funded` (idempotency key `rail|tap|{tap_ref}`) → insert presentment
(`kind='first'`, `arn='TAPP-'+tap_ref`, `authorization_id NULL`, current fee
schedule) → `clearing.OpenBatch(today)` + `Clearer.Run` (clears every
unbatched presentment, computes the split, records the intent) → post
`rail.merchant_paid` for this presentment → if the merchant is listed,
`buyback.New().RunSession(instrument, today)` (allocates every pending intent
if today's session has a price; otherwise they wait, and `RunSession` is
called again by the market close). Response `201` (or `200` on replay):
```json
{ "tap_ref": "tap_…", "presentment_id": "…", "fee_kobo": 5000,
  "buyback_funding_kobo": 500, "intent_id": "…",
  "intent_state": "allocated | pending | escrowed",
  "allocated_units": 12500000, "price_kobo": 4000,
  "symbol": "MAMAPUT", "lock_until": "2027-01-16" }
```
An unlisted merchant yields `intent_state: "escrowed"`, `symbol: null` — the
funding accrues (the listings pipeline), not an error.

`POST /v1/rail/taps/{tap_ref}/reverse` `{ "reason": "merchant_reversal" }` —
inserts a `full_reversal` presentment against the same card/merchant, clears it
(the fee engine negates the split), and if the intent was already allocated
unwinds it through `dispute`'s buyback unwind (the lot is still locked, so this
always succeeds). `200 { "state": "reversed", "unwound_units": … }`.

### Cardholders

`GET /v1/rail/cardholders/{cardholder_ref}/holdings`
```json
{ "cardholder_ref": "usr_…", "as_of": "2026-09-18",
  "total_value_kobo": 812500, "total_cost_kobo": 800000,
  "holdings": [ {
    "symbol": "MAMAPUT", "legal_name": "Mama Put Kitchens Ltd", "trading_name": "Mama Put",
    "units": 20312500, "shares": "0.203125",
    "sellable_units": 0, "locked_units": 20312500, "next_unlock": "2027-01-16",
    "cost_kobo": 800000, "reference_price_kobo": 4000, "value_kobo": 812500,
    "change_bps": 156, "lots": 3,
    "last_session": {"date":"2026-09-17","price_kobo":4000,"source":"auction"} } ] }
```
`GET /v1/rail/cardholders/{cardholder_ref}/holdings/{symbol}` — the one holding
with its `lots` (`units`, `cost_kobo`, `acquired_at`, `transferable_from`,
`tap_ref` when it came from a tap) and `prices` (last 90 adjusted
observations, same shape as `/v1/instruments/{symbol}/market-data`).

`GET /v1/rail/cardholders/{cardholder_ref}/activity?limit=` — allocated and
pending intents newest first: `tap_ref`, `symbol`, `funding_kobo`, `state`,
`units`, `price_kobo`, `at`.

### Market

`POST /v1/rail/market/close` `{ "session_date": "2026-09-18" }` (optional,
default today) — runs the close for every listed instrument: open the session
if it does not exist, `RunToSettlement`, surveillance, `buyback.RunSession`.
Returns one row per instrument (`state`, `price_kobo`, `matched_units`,
`buyback: {intents, units, escrowed, refusal}`). Idempotent: a published
session is skipped, buyback re-runs harmlessly. The same routine runs on a
schedule inside `exchanged` at `MARKET_CLOSE_AT` (default `12:00`, Africa/Lagos)
on trading days; `MARKET_CLOSE_AT=off` disables the scheduler (tests, and any
second replica).

`GET /v1/rail/market/today` — business date, whether it is a trading day, the
session phase, and per instrument: symbol, session state, reference, halted.

## Tapp: what changes

Branch `base-mainnet-cdp-gas-ngn` (production). Config: `FREEDOM_BASE_URL`,
`FREEDOM_RAIL_TOKEN` (both optional; when unset the equity feature is off and
nothing else changes).

1. **Business** — hand-written migration `sql/0020_business.sql`:
   `merchant_businesses(sender_id pk, legal_name, trading_name, rc_number, mcc,
   symbol, state submitted|listed|rejected, findings jsonb, freedom_instrument_id,
   reference_price_minor, submitted_at, decided_at)`. Routes (sender JWT):
   `POST /v1/sender/me/business` (body = the rail request minus refs; the API
   fills `merchant_ref` = sender profile id, `cardholder_ref` = the owning
   user id, forwards, stores the result), `GET /v1/sender/me/business` (stored
   record + live cap table from Freedom, merged), `GET
   /v1/sender/me/business/holders`. Money on the wire stays `money.Amount`.
2. **Outbox** — `sql/0021_equity_outbox.sql`: `equity_outbox(id, kind
   tap|reverse, tap_id, payload jsonb, state pending|delivered|failed, attempts,
   last_error, next_at, delivered_at, response jsonb)`. `tap.Service.Debit`
   inserts a `tap` row in the same transaction as the charge; the reverse
   handler inserts a `reverse` row. A ticker worker (`internal/equity`,
   started from `main.go` like the others, guarded by `DISABLE_BACKGROUND_JOBS`)
   delivers with exponential backoff, stores the response, and never blocks a
   tap. The transactions read model exposes `equity` on a tap:
   `{state, symbol, units, shares, price}` for the merchant app and admin.
3. **Holdings** — cardholder JWT routes: `GET /v1/me/holdings`, `GET
   /v1/me/holdings/{symbol}`, `GET /v1/me/equity-activity` — thin proxies to
   the rail, cardholder_ref = user id, money re-shaped to `money.Amount`.

## What this deliberately does not do

- Move any naira between Tapp and Freedom. The participant's net position is a
  number in Freedom's ledger and a line in `settlement_positions`; paying it is
  a treasury operation, not a code path.
- Reconcile Tapp's uncapped 0.5% fee with Freedom's ₦1,000 MSC cap. Above a
  ₦200,000 ticket the two differ; Freedom's figure is the scheme's, Tapp keeps
  the excess in `revenue`. Recorded here so it is a known break, not a surprise.
- Let a cardholder sell from the Tapp app. Selling is order entry on
  `/v1/orders` and needs a member + client account; that is the next door.

## As built (2026-09-18) — where the code differs from the text above

- `POST /v1/rail/businesses` for a company (by `rc_number`) that already has a
  live instrument returns **409** `{"error":"already_listed","symbol":…}` unless
  it is the same `merchant_ref` (then 200, the existing record). One live
  instrument per company is a partial unique index (migration 0015).
- A tap for a merchant Freedom has never seen creates a placeholder merchant
  row; onboarding completes it and **adopts its escrowed intents**.
- Reverse also posts `rail.tap_reversed` and `rail.merchant_repaid` so the
  hold and the participant's settlement return to zero.
- Market close **retries escrowed intents** (flips them to pending before the
  buyback runs) and reports `retried` per instrument. Buyback at ingest only
  when today's session is published; otherwise the intent waits as `pending`.
- `last_session` carries `source` (`auction`/`carry_forward`) and the observed
  price on zero-volume sessions. `GET …/businesses/{ref}` `halted` is
  `{"halted":bool,"reason"?}`.
- Tapp's business POST takes `reference_price` as `money.Amount`,
  `shares_authorised_units`, `daily_release_units`; holdings rows use `holding`
  and `lot_count`; activity is `{activity:[…]}` keyed by `tap_id`. Full shapes:
  `/Users/mac/tapp-platform/apps/api/docs/equity.md`.
- **The exchange sets the listing price** (LISTING-RULES §2.4, migration
  0017). `POST /v1/rail/businesses` `evidence` takes `net_assets_kobo` and
  `revenue_kobo` (audited; revenue trailing 12 months); both must be positive
  or the application is refused on `financials`. `reference_price_kobo` in the
  request is accepted for clients already deployed and **ignored**. The
  response's `reference_price_kobo` is the price the exchange set — fair
  value (`fair_value_kobo` = net assets + 1.0× revenue) over `shares_in_issue`,
  rounded down to the tick, never below one tick — recorded as the
  `listing_price` finding. A rejected application carries the same two
  fields, so the merchant sees what it would have listed at. Tapp still sends
  `reference_price`; it should stop, and send the two audited figures instead.
- Listings admitted before the rule are re-anchored under it from the console:
  `POST /console/api/instruments/{symbol}/reanchor` `{net_assets_kobo,
  revenue_kobo, reason}` (after the close; shares in issue must be set). The
  new reference is a `manual` price observation, so the public market row
  shows `price_source: "manual"` until the next session prints.
