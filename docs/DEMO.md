# The demo: one tap, and the cardholder owns the shop

*What was built on 2026-09-18, how to deploy it, and the script. Companion to
`docs/INTEGRATION.md` (the contract).*

## What exists now, per repo (nothing committed yet)

| repo | branch | what changed | verified by |
|---|---|---|---|
| `/Users/mac/freedom` | main | `internal/rail` + `/v1/rail/*` in `exchanged`, migrations 0014–0015, market-close scheduler, `Dockerfile` + `railway.json`, `make run` | `make check` green; the full story driven over HTTP on a from-empty database (below) |
| `/Users/mac/tapp-platform` | `base-mainnet-cdp-gas-ngn` | `internal/equity` (Freedom client + outbox worker), migrations 0020–0021, `/v1/sender/me/business*`, `/v1/me/holdings*`, `/v1/me/equity-activity`, `equity` on transaction rows, `docs/equity.md` | 44 packages green on a real Postgres; live test against Freedom |
| `/Users/mac/tapp` (cardholder PWA) | `feat-sui-cashout-offramp` | tokens + primitives reworked (Attio/Linear, Bricolage display), home/history/settings/sign-in recomposed, `/holdings`, `/holdings/[symbol]`, "Your shares" on home, equity line on tap rows | `yarn build`, `tsc` |
| `/Users/mac/tapp-merchant` (POS) | main | `app/(app)/business/*` (list your business, 4-step onboarding, holders register), dashboard + settings entry, equity line on receipts | `tsc`, lint at baseline; needs a new APK |

## What the numbers are

A ₦10,000 tap: MSC ₦50 (0.5%) → interchange ₦30, scheme ₦15, **buyback ₦5**
→ 0.125 shares at ₦40. With the merchant's 1% co-funding: ₦105 → 2.625 shares.
Tap-earned shares are locked 120 days. A reversed tap is unwound; the lot is
still locked so the unwind always succeeds. The participant (Tapp) owes the
scheme exactly the MSC. All of this was observed on a fresh database, not
asserted from a test fixture:

```
tap before listing      fee 5000  funding 500   escrowed  symbol null
business lists          listed MAMAPUT, 7/7 findings met, founder lot issued
same RC, 2nd merchant   409 already_listed
market close            MAMAPUT published @4000 (carry_forward) buyback intents=1 units=12500000
tap after close (1% cf) funding 10500  allocated 262500000 units  lock 2027-01-16
holdings                MAMAPUT 2.75 shares, locked, value ₦110
reverse                 unwound 262500000 → 0.125 remain
ledger (NGN)            settlement −50 = interchange 30 + revenue 15 + treasury_cash 5
```

## Deploy

Freedom must be reachable from the Tapp API on Railway, so it goes on Railway
too (Zerocard workspace, like Caelum). `railway` commands are yours to run.

1. **Freedom** — service `freedom` in the `tapp-platform` Railway project
   (Zerocard workspace), git-connected to `github.com/folajindayo/freedom`
   `main`; a push deploys. It shares the project's Postgres *instance* but not
   its database: `DATABASE_URL=${{Postgres.DATABASE_URL}}` + `DATABASE_NAME=freedom`
   makes `exchanged` create the `freedom` database on first boot and migrate it
   (both codebases own a `ledger_entries` table, so one schema cannot hold
   both). Also set: `RAIL_TOKEN`, `CONSOLE_TOKEN`, `MARKET_CLOSE_AT=12:00`
   (`off` to close by hand during the demo), `PORT=8081`. Public domain:
   `https://freedom-production-f31c.up.railway.app` (console at `/console`).
   Freedom does not use Redis.
2. **Tapp API** — `FREEDOM_BASE_URL=http://freedom.railway.internal:8081`
   (private network; the rail token never leaves Railway) and
   `FREEDOM_RAIL_TOKEN` are set on the `api` service; commit the branch,
   `railway up --service api --ci` from `/Users/mac/tapp-platform` (clean tree).
   Migrations 0020/0021 apply at boot; the outbox worker starts unless
   `DISABLE_BACKGROUND_JOBS` is set.
3. **Cardholder PWA** — `/Users/mac/tapp` targets `https://api.usetapp.xyz`
   (`.env.local`), the same Railway service. Deploy however that repo deploys
   today (`vercel deploy --prod` from its root; it has no `.vercel` link here).
4. **Merchant APK** — `/Users/mac/tapp-merchant`: `npx expo prebuild --clean &&
   npx expo run:android --variant release` on a connected device, or an EAS
   `preview` build. `app.json` already points at the Railway API.

## The script

1. **Merchant app → Your business → List your business.** Four steps: legal
   identity (RC number, symbol), shares (in issue, authorised, treasury pool,
   reference price, daily release cap, optional co-funding), evidence
   declarations, review. Submit. The findings come back one by one; state
   becomes *Listed* with the cap table live from the exchange.
2. **If it is before 12:00 Lagos** (or the scheduler is off): run the close so
   today's session has a price —
   `curl -X POST $FREEDOM/v1/rail/market/close -H "Authorization: Bearer $RAIL_TOKEN" -d '{}'`.
   After that every tap allocates immediately. (Taps before it wait, and the
   close allocates them — also a fine thing to show.)
3. **Tap.** Merchant app → Tap Card, ₦10,000. Receipt shows the amount; the
   transaction detail shows *Equity: customer earned 0.125 MAMAPUT at ₦40.00*
   within ~5 s (outbox tick).
4. **Cardholder PWA → home.** *Your shares*: Mama Put, 0.125 shares, ₦5.
   Open it: value, the 90-session price line, the lot with *unlocks on
   2027-01-16* and the one-sentence reason.
5. **Merchant app → Your business.** Treasury remaining, released today,
   holders, the customer in the register.
6. Optional: reverse the tap from the merchant app; the equity line flips to
   *reversed*, the cardholder's holding is gone.
7. **The public market** is at `$FREEDOM/market` — no sign-in. Open it on a
   phone: the listed business is there with its price, company value and
   holder count; tap the row for the price line, the day's session and the
   cap table. It is the link to send someone who asks "so what did I buy?".

## Market maker

Without one, a symbol nobody sells only ever carries its reference forward.
The house market maker (member `TAPP`, trading as cardholder `freedom:mm`) is
funded, given inventory and appointed from the console; from then on the
quoting engine puts a two-sided quote into every session at `MM_QUOTE_AT`
(10:05 Lagos), and a session with no trade publishes the quote's mid.

1. **Console → Members → TAPP → Fund market maker.** Say ₦200,000, reason
   "launch capital". Cash moves from the scheme's float to the market maker's
   account, one ledger transaction, your name on it.
2. **Console → Instruments → MAMAPUT → Place with market maker.** Say 2,000
   shares, reason "launch inventory". The block is sold out of the treasury at
   the reference (₦40 → ₦80,000), the lot and cap table event are written, and
   a *Market maker placement* notice publishes. (A new listing can do this at
   admission: `market_maker_placement_units` on the business request.)
3. **Console → Instruments → MAMAPUT → Appoint market maker.** Member `TAPP`,
   target inventory blank (= what it holds). The exchange refuses a related
   party of the issuer.
4. **Console → Overview → Run quote** (or wait for 10:05). The Quotes tab on
   the instrument shows today's bid/ask, sizes, centre, skew and the last 20
   sessions.
5. **Run close.** Nothing crosses — the market maker does not trade with
   itself — so the session publishes the mid: the public market row shows
   *market maker's quote* as the price source, the reference has moved, and
   the next tap buys at it. Sell the customer's shares into the market
   maker's bid from `/v1/orders` and the next close prints a real trade.

Turn it off with `MM_QUOTE_AT=off`; run it by hand with
`POST /console/api/market/quote`.

## Known limits, stated plainly

- Selling is not in the cardholder app yet (needs a member + client account on
  `/v1/orders`). Holdings are real, sellable after the lock, via the exchange API.
- Tapp charges 0.5% uncapped; Freedom's MSC caps at ₦1,000. They differ above
  a ₦200,000 ticket. Freedom's figure is the scheme's.
- Merchant co-funding is booked in Freedom's ledger as owed by the participant
  (Tapp). Tapp's payout to the merchant does not yet deduct it, so a co-funding
  merchant is, for now, funded by the scheme's position rather than its own
  payout. Deducting it in Tapp is the next change on the money path.
- The exchange operations console is at `/console` on the Freedom service
  (sign in with your name and `CONSOLE_TOKEN`). It reads the live database and
  its actions call the engine (halt/release, triage/close alerts, publish
  disclosures, graduate/demote, member kill switch, run the close, fund /
  place / appoint the market maker, run the quote).
- The house market maker is the sponsor member (`TAPP`) wearing a second
  role. LISTING-RULES §7 names that conflict; the quote is a formula on the
  record, not a trader's discretion, which is the mitigation for now.
- The Mac's disk was at ~99% during this work; two builders had to clear caches.
