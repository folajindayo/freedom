# Freedom Exchange — Listing Rules

*Non-composite securities exchange · Nigeria · v0.1 draft*

An exchange is a rulebook with a matching engine attached. This is the rulebook.
It is the document the SEC licenses, and it decides which merchants a cardholder
can end up owning — so it is a product decision as much as a legal one.

> **Status: draft for legal review.** Nothing here has been settled with counsel
> or filed. Figures marked ⚠ are placeholders chosen to be argued with.

---

## 1. What this market is for

Freedom lists Nigerian small and medium businesses that accept the Freedom card.
Almost none of them would ever meet the NGX main board's standards, and that is
the point: without a market for their shares, the card's promise — that spending
at a shop makes you part of it — cannot be kept.

The standards below are therefore lighter than a main board's and heavier than
none. Every one of them exists because its absence would let a specific thing go
wrong, and that reason is given.

## 2. Admission

### 2.1 The issuer

- Incorporated in Nigeria, registered with the CAC, **at least 24 months** of
  trading history. ⚠
- Audited accounts for the most recent financial year, signed by two directors,
  audited by a firm on the SEC's list.
- No director disqualified, undischarged bankrupt, or subject to an unresolved
  SEC action.
- A registered office and a named **company secretary** who is the exchange's
  point of contact and is answerable for disclosure.

*Why 24 months:* a business with no trading history has no basis for a price,
and the auction would be discovering a number rather than a value.

### 2.2 The securities

- Ordinary shares only, fully paid, freely transferable, one class.
- **Minimum free float: 10%** of shares in issue, held by at least
  **25 unrelated holders**. ⚠
- A **treasury pool** reserved for the buyback, with a board resolution
  authorising its release and a stated maximum daily release.
- Authorised share capital sufficient to cover the treasury pool without
  further authorisation for at least 24 months of projected card volume.

*Why a float at all:* the buyback needs somewhere for a price to come from. A
symbol whose only holders are the founder and the scheme has no market, and its
auction would be the scheme trading with the issuer.

*Why 25 unrelated holders:* it is the number the liquidity gate later measures,
and setting it at admission means a listing is not admitted to a standard it can
never graduate under.

### 2.3 A merchant relationship is not an entitlement

Accepting the Freedom card does not entitle a business to list. Listing is a
separate application, separately assessed. The card relationship is what makes
the listing *useful*; it is not what makes it *appropriate*.

### 2.4 The listing price

**The exchange names the listing price; the applicant supplies audited net
assets and revenue.**

An applicant does not propose a price, because an applicant cannot be trusted
to. The first version of the onboarding form let the founder type a number, and
one typed ₦100,000 a share against ten million shares: a ₦1 trillion valuation
for a business the size of a kitchen. Nothing about that is fraud — it is what
anyone does when asked to value their own company — and that is why the
question must not be asked. The number that opens the first auction is the
exchange's, derived from figures an auditor has signed, and the applicant's
opinion of it is recorded nowhere.

The price is fair value over the shares in issue:

```
FairValue    = NetAssets + RevenueMultiple × Revenue
ListingPrice = FairValue / SharesInIssue
```

- *Net assets* and *revenue* are taken from the audited accounts §2.1 already
  requires: net assets at the balance sheet date, revenue for the trailing
  twelve months. An applicant without them has no fair value, so has no price,
  so cannot list — this is a separate finding (`financials`) beside the
  audit one, so a refusal says which was missing.
- *RevenueMultiple* is **1.0×**. ⚠ It is a placeholder chosen to be argued
  with, and one number for every sector is plainly crude; a restaurant and a
  pharmacy do not turn revenue into value at the same rate. It is a criterion,
  not a constant in code, so changing it is a rulebook decision made in the
  open rather than a deployment.
- The division rounds **down** to the instrument's tick, so the price never
  overstates the accounts, and is never below **one tick**, so a company whose
  fair value is below a kobo a share still has a number the auction can move
  away from.

The price is a starting point, not a verdict. The auction discovers the value
from the first session; this rule only decides where discovery starts, and it
starts from the accounts rather than from hope. The figures the decision was
made from — fair value, net assets, revenue, the multiple, the share count and
the resulting price — are written on the application beside the other
findings, so the answer to "why ₦8?" is on the record and not in someone's
memory.

A listing admitted before this rule existed is re-anchored under it by the
exchange, after a close and never during a session, from the same audited
figures: the new reference is written as a manual price observation and as a
further `listing_price` finding naming who did it and why.

*What the revenue anchor is meant to become:* the audited revenue figure is
the best evidence available on the day a business lists, and the worst
available a year later, because by then the exchange has watched the business
trade. The intent of the rule is that after **90 sessions** ⚠ the revenue
anchor moves from the audited accounts to the merchant's **annualised card
turnover through the scheme** — revenue the exchange has itself settled and
cannot be told stories about. That re-anchoring is **not yet implemented**;
today the audited figure is the only one used, and the fair value on the
record is the one computed at admission.

## 3. Continuing obligations

### 3.1 Disclosure

- **Audited annual accounts** within 120 days of year end. ⚠
- **Half-year results**, unaudited, within 60 days. ⚠
- **Material events** — anything a reasonable holder would want to know before
  trading — announced **before the next auction**, and in any case within 24
  hours of the board becoming aware.
- **Director dealings** within 5 business days.
- **Shareholding changes** crossing 5%, within 5 business days.

Announcements are submitted to the exchange and published to every holder at one
instant. Selective disclosure — telling anyone before everyone — is the offence,
and the exchange halts trading between submission and publication precisely so
that the window in which the issuer knows more than the market is closed rather
than merely discouraged.

### 3.2 Closed periods

Directors, staff, affiliates and the issuer itself may not trade the symbol:

- from the end of a financial period until the results are published;
- from the moment a corporate action is contemplated until it is announced;
- whenever the exchange declares a closed period.

On an auction-only symbol related parties may not trade **at all**. The closed
period is the narrower rule that applies once a symbol graduates to continuous
trading and loses that blanket ban.

### 3.3 Corporate actions

Splits, bonus issues and dividends are announced with **ex-date, record date and
pay date** at least 10 business days ahead. ⚠ The exchange adjusts the reference
price and the historical price series on the ex-date; an issuer that fails to
announce in time will find its adjustment applied late and its band wrong, which
is its own problem and its holders' loss.

### 3.4 Fees

| | |
|---|---|
| Application | ⚠ |
| Admission | ⚠ |
| Annual listing | ⚠ |
| Corporate action processing | ⚠ |

To be set with reference to the SEC's fee schedule and to what an SME can
actually bear. A listing fee that only a large company can pay defeats the
market's purpose.

## 4. Suspension and delisting

### 4.1 The exchange suspends trading when

- an announcement is pending (automatic, lifted on publication);
- accounts are overdue;
- the free float falls below 10% for 30 consecutive days; ⚠
- a blocking surveillance alert is open;
- a data-quality incident is unresolved;
- the issuer asks, with reasons the exchange accepts.

### 4.2 Delisting

After **90 days** of continuous suspension, or on: ⚠

- failure to file accounts for two consecutive years;
- the free float falling below 5% with no remedy; ⚠
- a court winding-up order;
- a false statement in listing particulars or an adverse audit opinion;
- the issuer's request, with holder approval.

### 4.3 What happens to holders on delisting

This is the question the rulebook has to answer honestly, because the cardholder
who earned those shares by buying lunch did not choose to take this risk.

- A final call auction runs, giving holders one opportunity to sell.
- The issuer **must** offer to buy back at no less than the trailing 20-session
  volume-weighted average price. ⚠
- Unsold holdings remain on the register. They are not cancelled, and the
  registrar continues to record them and pass on any dividend.

## 5. The liquidity gate

A symbol is admitted **auction-only** and trades in one call auction per session.
Continuous trading is earned, not granted, because a continuous book on an
illiquid name manufactures a price rather than discovering one.

Over a trailing 20 sessions, all of:

| | |
|---|---|
| Sessions that crossed | ≥ 15 of 20 |
| Median session notional | ≥ ₦500,000 ⚠ |
| Distinct **unrelated** buyers | ≥ 20 |
| Distinct **unrelated** sellers | ≥ 10 |
| Largest single account | ≤ 25% of volume |
| Largest related-party group | ≤ 40% of volume |
| Largest session-over-session move | ≤ 20% |
| Sessions since listing | ≥ 60 |
| Open blocking alerts or incidents | none |

Every criterion is either a count of distinct unrelated participants or a
concentration limit, because those are the ones an issuer cannot satisfy by
trading with itself.

**Graduation is reversible.** Failing any of the first five for 10 consecutive
sessions returns a symbol to auction-only, with resting orders cancelled and
their reservations released. A one-way door removes the issuer's incentive to
maintain liquidity the moment they are through it.

## 6. Related parties

The issuer, its treasury, its directors, its staff, its affiliates and the
merchant itself are **related parties**, recorded per symbol and enforced at
order entry. Their channels into the market are:

- the **treasury release** that funds the buyback — a price taker, executing at
  the price the auction discovered, capped in units per session;
- a **bid of last resort**, if the issuer offers one — also a price taker,
  priced at or below the prior reference, capped in units, and excluded from
  price formation.

A designated market maker must be **independent of the issuer**. An obligation
to quote, held by a party that benefits from the price, is the manipulation
vector wearing a badge.

## 7. Sponsors

Each listing retains a **sponsor** — a firm answerable to the exchange for the
issuer's compliance — for at least the first 24 months. ⚠ The AIM model: a
company too small for a full listing apparatus borrows one.

Where Freedom itself sponsors a listing it has admitted, to a market it
operates, in a company whose shares its own buyback purchases, that conflict is
real and must be managed by disclosure and by separation of the listings
function from the trading function. It should not be waved away.

## 8. Open questions for counsel

Superseded by `docs/REGULATORY.md` §9, which re-orders these against the
Act's text and adds the two that matter most: that every listed issuer must be
a **public company with SEC-registered securities** (ISA 2025 s.95, s.97(1)(d))
— which §2.1 above does not yet require — and that the internal DVP is a
securities settlement system by the Act's own definition (s.357), so the ₦5bn
question is about *which* registration, not whether one is needed.

---

*Cross-references: `docs/REGULATORY.md` for licences, capital and counsel questions;
`docs/SECURITY.md` for market integrity controls;
`internal/exchange/graduation.go` for the liquidity gate as implemented;
`internal/exchange/admission.go` (`ListingPrice`) for §2.4 as implemented;
`internal/institution/` for disclosure, complaints and investor protection.*
