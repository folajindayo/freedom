# Freedom

A card scheme, a merchant acceptance network, and a private stock exchange —
built together, because the product only works if all three exist.

**Every tap buys the cardholder a slice of the business they just spent at.**
The fee pool funds it. And because essentially no Nigerian SME retailer is
listed anywhere, there is no market in which to buy those shares — so the
exchange is not a second product, it is the thing that makes the first one
possible.

```
tap (NTAG) → softPOS → acquirer → switch → issuer (hold) → approved
                                                              ↓
                                              clearing (T+1), fee split
                                                              ↓
                                          buyback allocator → exchange
                                                              ↓
                                            shares in the stock wallet
```

That path runs today, and so does the way back out. `make e2e`:

```
one ₦10,000 tap at SHOP18A936: the cardholder now owns 0.125 shares
tapped ₦10,000 → earned 0.125 → sold 4 shares at ₦40.00 → ₦155.00 in hand
session printed ₦80.00, buyback paid ₦40.00
```

The last line is the wash-trading defence: a listing that ramps its own price
on a thin session does not get paid the ramped price.

## The economics, stated plainly

At a CBN-style 0.5% card-present rate, a ₦10,000 tap generates ₦50 of merchant
service charge, and 10% of that pool is **₦5 of stock**. Real accumulation, but
not a wealth engine. The cap makes it worse at scale: a ₦1,000,000 tap is capped
at ₦1,000 of MSC, so it yields ₦100 — not ₦5,000.

The lever that changes the picture is **merchant co-funding**. A merchant opting
into a 1% equity rebate turns that same ₦10,000 tap into **₦105 of stock**, and
for them it beats a cash discount outright: they are paying in their own treasury
shares to buy a customer who now owns a piece of the shop. Expect it to become
the dominant funding source. Both numbers are pinned by tests in
`internal/fee/fee_test.go` and `internal/e2e/network_test.go`.

## Layout

```
apps/api/                       one Go module, one binary per service
  internal/
    money/ share/ alloc/        integer minor units and exact splits
    ledger/                     multi-asset double-entry; naira and equity
    tapcrypto/                  NTAG215 and NTAG424 DNA credentials
    scheme/                     business date, cutover, decline codes
    issuer/ acquirer/           authorisation, capture, refund
    clearing/                   T+1 batch, fee split, buyback intents
    buyback/                    allocation, treasury release, price band
    exchange/                   call auction, reservations, T+0 settlement
    migrate/sql/                embedded schema
    e2e/                        the whole rail, end to end
docs/SECURITY.md                threat model, and what is NOT defended
```

## Running it

```sh
make db      # database + the two Postgres roles
make test    # everything
make e2e     # just the golden path, verbosely
make check   # what CI runs
```

Requires Go 1.26 and Postgres 15. Docker is a fallback if no local Postgres is
listening.

## Design decisions worth knowing before you read the code

**Naira and shares are different Go types.** `money.Kobo` and `share.Units` are
both int64 underneath, and deliberately not interchangeable — the buyback
allocator holds both in the same function, and a bare int64 for each would let
the compiler watch you add one to the other. No floats touch either, ever.

**The ledger balances per asset, not overall.** A buyback is one event with two
independently balanced legs. A deferred constraint trigger enforces it in the
database, so an unbalanced write cannot commit even by a bug.

**A committed ledger transaction can never be amended.** Entries may only be
added by the transaction that opened their header. Corrections are new events —
which is also what settlement finality means to a participant reconciling a
statement.

**The buyback fires at clearing, never at authorisation.** An authorisation can
be reversed, and a reversed tap must not have bought anyone shares.

**The buyback is a price taker.** It buys from treasury at the price a call
auction discovered and is never an input to that auction. Otherwise the network's
own demand sets the price the network pays.

**An auction price is one someone named.** Never a band edge. A book of market
orders on both sides ties at every price, and a ladder without this invariant
picks the top of the band — one 0.01-share trade printing at reference × 1.20 and
becoming tomorrow's buyback price. When the reference sits inside the tied
interval, the reference itself is the price.

**Trading settles T+0, atomically.** Cash and shares are reserved at order entry
and swapped in one ledger transaction. There is no unsettled window, so there is
no clearing house, no netting, no margin and no guarantee fund — all of which
exist only to manage a window we do not have. The cost is that nobody trades on
credit, which for prepaid cardholders is not a constraint.

**Shares earned by tapping cannot be sold for 120 days.** Otherwise a fraudster
taps, takes the equity, sells it, and charges the tap back. Reservations pick
lots by identity rather than quantity, so a locked lot cannot be sold even when
the account's total looks sufficient.

**Sub-kobo residuals belong to the cardholder.** They carry forward to the next
buyback rather than being swept to revenue. A scheme that quietly keeps the
change from millions of taps has invented a fee it never disclosed.

**NTAG215 is not authentication, and the code says so.** See
[docs/SECURITY.md](docs/SECURITY.md) §2. The pilot credential makes cloning
loud rather than impossible; that limit is asserted by a test so a future change
that appears to fix it without moving to NTAG424 reads as wrong.

## Status

Built: the ledger, fee engine, both credential technologies, authorisation,
capture, clearing, the buyback, and the exchange — call auction, pre-trade risk,
T+0 settlement, FIFO cost basis, price bands and the wash-trading cap. Proven
end to end with 95 tests including database-level concurrency, a brute-force
matching oracle, and fuzzing.

Not built: the continuous order book (symbols are auction-only until they earn
it), designated market makers, corporate actions, chargeback unwind, dispute
clocks, settlement to member banks, the softPOS app, and the consoles.
`docs/SECURITY.md` §10 lists the security-relevant gaps.
