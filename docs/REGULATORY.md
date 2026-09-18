# Freedom — Regulatory Surface

*v0.1 draft · researched 2026-09-17 · for counsel, not a substitute for counsel*

Freedom is three regulated things wearing one product: a card scheme, an
acquiring network, and a securities exchange with its own settlement. Each is
licensed by a different regulator, capitalised separately, and fails
differently. This document maps every regulated function the system performs to
the licence that permits it, the capital that licence now costs, and the
obligations that come with it — so that the ₦-figure on each decision is visible
before it is made.

> **Status: draft.** Everything below is read from primary sources cited in
> §10 — the Investments and Securities Act 2025 as gazetted, SEC Circular 26-1,
> CBN circulars — and from secondary commentary where the primary text was not
> reachable. Section numbers are from the Act. Nothing has been confirmed with
> the SEC or CBN. Figures marked ⚠ are ones the business should challenge.

---

## 1. The regulators and what each one owns

| Regulator | Instrument | Which part of Freedom it governs |
|---|---|---|
| **CBN** | BOFIA; PSP licensing framework (2020); Contactless Payments Guidelines (2023); Guide to Charges (2026) | Card issuance, acquiring, switching, terminals, the MSC, contactless limits, the cash rail |
| **SEC Nigeria** | Investments and Securities Act 2025; SEC Rules; Circular 26-1 (minimum capital) | The exchange, settlement, the register, the buyback's distribution of shares, disclosure, the investor protection fund |
| **CAC** | CAMA 2020 | Whether a listed merchant is a company that may have its shares dealt in publicly at all (§4) |
| **FIRS / state IRS** | CITA, PITA, CGT Act, Finance Acts | Tax character of shares earned by tapping; capital gains on sale |
| **NDPC** | Nigeria Data Protection Act 2023 | BVN/NIN, transaction data, the holder register |

The first two are the ones that decide whether the business can exist. The rest
decide what it costs.

## 2. Every regulated function Freedom performs

Stated as functions, not as products, because that is how a regulator reads the
system. Each row is something the code already does.

| Function | Where in the code | Regulated as | Regulator |
|---|---|---|---|
| Issue a payment instrument (the tag) | `tapcrypto/`, `issuer/` | Card issuer — must be a CBN-licensed bank or MMO | CBN |
| Accept payments on a merchant's phone | softPOS (not built), `acquirer/` | Acquirer (bank) + PTSP + PSSP; MPoC/CPoC certification | CBN, PCI |
| Route authorisations between issuer and acquirer | `switch` in the tap path | Switching and processing | CBN |
| Clear card transactions T+1, split fees | `clearing/` | Part of switching/scheme operation | CBN |
| Move naira in and out for members | `cashrail/` | NIBSS participant via a settlement bank | CBN |
| Bring together buyers and sellers of shares, match them | `exchange/` | **Securities exchange** (s.26–27) | SEC |
| Transfer shares and cash by book entry against each other | `ledger/`, `exchange/` T+0 DVP | **Financial market infrastructure** — securities settlement system / clearing and settlement company (s.41; definitions s.357) | SEC |
| Hold cardholders' shares in pooled accounts | `share/`, `registrar/` | Nominee company; custodian if assets are held as bailee | SEC |
| Maintain the register of members for each issuer | `registrar/` | **Registrar** / securities transfer agent (s.357) | SEC |
| Distribute an issuer's shares to the public in return for spending | `buyback/` | Invitation to the public to acquire securities (s.95–97) | SEC |
| Publish a price, an index, statements | `institution/` | Exchange functions; market data | SEC |
| Compensate investors when a member fails | `institution/` protection fund | **Investor protection fund** (s.198–216) | SEC |
| Surveil and discipline members | `surveillance/` | Self-regulatory function of the exchange (s.30, 33) | SEC |

Thirteen functions, at least nine distinct registrations. Whether Freedom holds
them all or rides on someone else's is the structuring question in §8.

## 3. What each licence now costs

SEC Circular 26-1 (16 January 2026) replaced the 2015 capital rulebook. The
numbers below are the revised minimum capital; the compliance deadline for
existing operators is **30 June 2027**, and a new registrant should assume the
revised figure applies from day one. Registration fees are from the SEC
checklists and are separate from capital.

### 3.1 SEC

| Registration | 2015 minimum | **Revised minimum** | Registration fee | Notes |
|---|---|---|---|---|
| Non-composite securities exchange | ₦500m | **₦5.0bn** | ₦30m + ₦100k filing + ₦300k processing | Fidelity bond ≥ 25% of minimum capital; 4 sponsored individuals; annual renewal by 31 January |
| Composite securities exchange | ₦500m | ₦10.0bn | — | Not needed: Freedom lists one asset class |
| Clearing and settlement company (CSC) | ₦200m | **₦5.0bn** | — | "Intermediary in making payments or deliveries in connection with transactions in securities" |
| Central counterparty (CCP) | ₦5.0bn | ₦10.0bn | — | Not needed: Freedom never novates; fills are pre-funded by reservation |
| Registrar | ₦150m | **₦2.5bn** | ₦1m | Fidelity bond ≥ 20%; 3 sponsored individuals |
| Nominee company | ₦1,000 | **₦5m** | — | Holding vehicle only |
| Non-bank custodian | — | ₦50bn + 0.1% AUC | — | Avoid — use a bank custodian ("as prescribed by CBN") |
| Crowdfunding intermediary | ₦100m | ₦200m | — | Relevant only under option C in §8 |
| Broker-dealer | ₦300m | ₦2.0bn | — | Members of the exchange bear this, not Freedom |
| Sub-broker (digital) | ₦10m | ₦100m | — | The plausible licence for the stock wallet if it is not itself a member |

The exchange penalty for operating unregistered is not less than the prescribed
paid-up capital plus ₦100,000 a day (s.26(3)); for an FMI it is not less than the
prescribed capital or five years' imprisonment (s.41(2)). The capital numbers are
therefore also the floor of the fine.

### 3.2 CBN

| Licence | Minimum capital | What it permits |
|---|---|---|
| Switching and processing | ₦2.0bn | Routing between issuers and acquirers; card scheme operation in practice |
| Mobile money operator | ₦2.0bn | Issuing wallets — the only non-bank route to being an issuer |
| Payment solution services (PSS, combined) | ₦250m | Super-agent + PTSP + PSSP together |
| Payment terminal service provider (PTSP) | ₦100m | Deploying and maintaining terminals — softPOS included |
| Payment solution service provider (PSSP) | ₦100m | Gateways, processing on behalf of merchants |
| Super-agent | ₦50m | Agent networks |

Only CBN-licensed institutions may act as issuer or acquirer under the
Contactless Payments Guidelines. Freedom cannot be either without a bank or MMO
licence; it can be the scheme, switch, and PTSP/PSSP around a partner bank.

### 3.3 The stack, totalled

| Structure | SEC capital | CBN capital | Total regulatory capital |
|---|---|---|---|
| Everything in-house (exchange + CSC + registrar + nominee; switch + PSS) | ₦12.5bn | ₦2.25bn | **≈ ₦14.8bn** |
| Exchange + registrar + nominee; settle through an existing CSD; PSS only, partner switch | ₦7.5bn | ₦250m | **≈ ₦7.8bn** |
| Exchange + nominee only; external registrar and CSD; PSS only | ₦5.0bn | ₦250m | **≈ ₦5.3bn** |
| No exchange licence — crowdfunding-shaped (§8, option C) | ₦200m | ₦250m | **≈ ₦450m** |

The 2015 numbers would have made the second row ≈ ₦900m. The revision is a
tenfold change in the cost of being an exchange, and it landed eight months ago.
Any plan written before January 2026 is wrong by that factor.

## 4. The finding that reorders everything: listed merchants must be public companies

This was not in the previous list of questions, and it should have been first.

**s.95(1):** no person may make an invitation to the public to acquire
securities unless it is "a public company and the securities it seeks to offer
to the public have been registered with the Commission" (or a bank, a CIS, a
government body, or a free-zone entity).

**s.97(1)(d):** an invitation is to the public if it is "made to one or more
persons to acquire securities dealt in by a securities exchange."

Read together: the moment a merchant's shares are dealt in on Freedom Exchange,
every allocation of those shares — including the buyback — is an invitation to
the public, and s.95 permits that only for a **public company** whose securities
are **registered with the SEC** (s.86). A private limited company cannot be
listed on a securities exchange under this Act, whatever the exchange's own
listing rules say. The penalty for breach is at least 10% of the gross value of
securities distributed (s.95(4)), and every recipient may rescind (s.95(5)).

Nearly every Freedom merchant is a private limited company. So the listing
rules' admission standard (`LISTING-RULES.md` §2.1) is missing the one
requirement the statute makes non-negotiable:

> The issuer is a **public company** under CAMA, and its shares are
> **registered with the Commission** under s.86 before admission.

What this costs a merchant: re-registration as a public company at the CAC, the
public-company obligations in Part IX B (annual and periodic filings with the
SEC, s.88; internal control system, s.89; SEC-registered auditor, s.90), and a
registered prospectus or a s.104 exemption certificate for the initial
distribution. It does not require a main-board listing — NASD OTC has operated
for a decade as exactly this: a non-composite venue for unlisted *public*
companies. That is the precedent Freedom Exchange should be described against.

Two consequences for the product:

1. The onboarding funnel has a legal step in it that the code does not model.
   `listing/admission` needs an issuer status of *public company, securities
   registered* as a hard gate, with the CAC and SEC reference numbers recorded.
2. The 24-month trading history in the listing rules is now the smaller
   hurdle. The bigger one is whether a shop owner will convert to a plc to be on
   the network. Sponsors (`LISTING-RULES.md` §7) exist to carry exactly this
   burden, and the sponsor fee is where its cost lands.

The alternative — not being a securities exchange at all — is option C in §8.

## 5. The ₦5bn question, answered as far as the text allows

*Does an internal DVP inside a nominee structure constitute "clearing and
settlement" requiring a separate ₦5bn licence, or does the nominee's ₦5m suffice?*

The Act's own definitions (s.357) settle more of this than the previous draft
assumed:

- A **securities settlement system** is "an entity that enables securities to
  be transferred and settled by book entry according to a set of predetermined
  multilateral rules." That is a description of `exchange/settlement` and the
  ledger it posts to.
- A **clearing and settlement company** is "any corporate body which acts as an
  intermediary in making payments or deliveries or both in connection with
  transactions in securities." Freedom's ledger is the intermediary in every
  fill.
- A **financial market infrastructure** is "any entity set up to carry out
  centralised, multilateral clearing, settlement ... or provide a platform for
  trading securities," and includes securities settlement systems. Operating one
  unregistered is a criminal offence (s.41).
- A **nominee** is not defined as performing any of these. It holds; it does
  not settle.

So the honest reading is: **the internal DVP is a securities settlement system
by definition, the nominee's ₦5m does not cover it, and the question is not
whether it is regulated but under which registration.** Three routes:

| Route | Capital | What changes in the code |
|---|---|---|
| **A. Register as a CSC** | ₦5bn | Nothing. The ledger is the settlement system; it needs an FMI rulebook approved under s.44 and default rules under Part V C. |
| **B. Settle through the existing CSD** (CSCS) as a participant, with Freedom as exchange only | ₦0 additional; CSCS fees | Real. Holdings live at the depository, not in `ledger/`; `share/` becomes a mirror; T+0 DVP becomes the CSD's settlement cycle unless CSCS offers intraday; the nominee holds an omnibus account at CSCS. Also satisfies s.122 (all secondary-market securities must be dematerialised) without argument. |
| **C. Seek a no-action position** that the exchange's registration encompasses settlement of its own trades | ₦0 if granted | Nothing — but the ₦5bn is the downside if the answer is no, and s.41(2) makes it a criminal downside. |

Recommendation for counsel: pursue **B** as the base case and ask whether the
SEC would treat an exchange-operated settlement layer as covered by the exchange
registration (C) where the CSD holds the securities and Freedom only nets. Route
A is what the architecture was built for and is the cleanest, but ₦5bn of
capital to avoid a CSCS integration is not a trade the business should make
before it has volume.

The corollary is that `CONTINUITY.md` §3's "Postgres is the only stateful
component" stops being true under route B. That is a real cost and should be
weighed as one.

## 6. The card rail: what changed in 2026

### 6.1 Merchant service charge

The **Guide to Charges by Banks and Other Financial Institutions, effective
1 May 2026**, sets the MSC at **0.5% of transaction value, capped at ₦10,000**,
regardless of channel. The 2020 guide's ₦1,000 card-present cap is gone.

`fee.SchemeV1` still has `mscCap := money.Naira(1_000)`. The comment above it
asks for exactly this verification, and the answer is that the cap has moved by
10×. The economics in `README.md` change with it: a ₦1,000,000 tap now yields
₦5,000 of MSC and ₦500 of stock, not ₦100. Sublinearity begins at ₦2m instead
of ₦200k. The number is regulated, so the change is not optional — but it also
changes what merchant co-funding is worth relative to the pool, and that
deserves a look before the constant is edited.

### 6.2 Contactless

CBN Contactless Payments Guidelines (2023) and the transaction-limits circular of
27 June 2023:

- **₦15,000 per transaction and ₦50,000 cumulative per day** without cardholder
  verification. Above that, PIN, mobile code or biometric.
- Contactless must be **opt-in**; it cannot be enabled by default.
- Only CBN-licensed institutions may be issuers or acquirers.
- Operators must hold and maintain PCI DSS, PCI PIN/PED, EMV and ISO 27001
  certifications and scheme-specific certification; terminals must meet the
  contactless specification.
- Round-the-clock dispute support, escalating to CBN.

The code has `cards.per_txn_cap_kobo` and `daily_cap_kobo`; the regulatory
ceilings for no-CVM taps are ₦15,000 and ₦50,000 and should be the defaults. A
tap above ₦15,000 needs a CVM step in the softPOS flow that does not exist yet.

### 6.3 Certification

MPoC (or CPoC for tap-only) is the PCI standard for accepting on a merchant's
phone. It is not started (`SECURITY.md` §10). Under the contactless guidelines it
is a condition of the acquirer's compliance, which means a partner bank will
require it before deploying the softPOS at all.

## 7. Continuing obligations — the recurring list

What has to happen on a schedule once licensed. These are the regulatory
counterparts to the operational cadence in `CONTINUITY.md` §6.

| Obligation | Frequency | Source | Who |
|---|---|---|---|
| Surveillance and enforcement report to the SEC | **Quarterly** | s.30(3) | Exchange |
| Notify SEC of disciplinary action against a member | Within **7 days** | s.33 | Exchange |
| Notify SEC of anything posing systemic risk | Immediately | s.30(1)(d) | Exchange |
| Notify SEC of insolvency proceedings (own or a member's) | Immediately | s.30(1)(e) | Exchange |
| SEC approval before amending rules or listing requirements | Per change | s.32 | Exchange |
| SEC approval of CEO and principal officer appointments | Per appointment | s.29 | Exchange |
| Annual registration renewal | By **31 January** | SEC checklist | Exchange, registrar |
| Fidelity insurance bond in force | Continuous | SEC checklist | Exchange (25%), registrar (20%) |
| Investor protection fund at or above minimum | Continuous; top-up on shortfall | s.198, s.208–209 | Exchange, trustees |
| IPF accounts and audit | Annual | s.206 | Trustees |
| Meet revised minimum capital | By **30 June 2027** | Circular 26-1 | All registrations |
| Issuers' annual and periodic reports to SEC | Annual + periodic | s.88 | Each listed merchant; exchange monitors |
| Legal entity identifier for every party to a securities transaction | Continuous | s.123 | Exchange, members, issuers, nominee |
| No cash; all secondary-market securities dematerialised | Continuous | s.121, s.122 | Exchange |
| Contract note / transaction confirmation per trade | Per trade | s.124–125 | Dealer (the stock wallet) |
| Verify MSC against the current Guide to Charges | On each revision | CBN | Scheme |
| Contactless limits in line with current circular | On each revision | CBN | Issuer |
| PCI / MPoC attestation | Annual | PCI SSC | Acquirer, Freedom |
| Data protection audit filing | Annual | NDPA 2023 | Freedom |

Most of these are events the system already produces (disciplinary actions,
halts, surveillance cases) and lacks only a filing. The quarterly surveillance
report in particular should be a query over `surveillance/` and not a document
somebody writes.

## 8. Structuring options

The three shapes the business can take, with what each one buys.

**A — Full stack.** Non-composite exchange, CSC, registrar, nominee, switch,
PSS. ≈ ₦14.8bn. Everything the code does is licensed in Freedom's own name.
Nothing about the architecture changes. The cost is the entire point.

**B — Exchange with borrowed infrastructure.** Non-composite exchange and
nominee in Freedom's name; settlement and depository at CSCS; register kept by
an SEC-registered registrar under contract; a partner bank as issuer and
acquirer; Freedom as PTSP/PSSP and scheme. ≈ ₦5.3bn, dominated by the exchange's
₦5bn. The code changes: `share/` mirrors the CSD, `registrar/` becomes an
interface to the appointed registrar, T+0 becomes whatever CSCS supports. This
is the shape NASD OTC actually has.

**C — Not an exchange.** Freedom registers as a **crowdfunding intermediary**
(₦200m) and a sub-broker (digital) (₦100m). Each merchant raises through the
portal under the SEC crowdfunding rules — private companies permitted, up to
₦50m/₦70m/₦100m per twelve months by size, retail investors capped at 10% of
annual income, one-year lock-in. The buyback allocates crowdfunded shares rather
than treasury shares. What it costs: **the secondary market**. Crowdfunding
rules assume investors "may never be able to sell," and any organised secondary
trading brings the venue straight back into s.26. The 120-day lock and the
unwind still work; the call auction, CLOB, market makers and index do not
exist. That is roughly half of what has been built.

C is the only shape that avoids the public-company conversion in §4, and the
only one under ₦1bn. It is also a different product: a loyalty scheme that
pays in illiquid shares. Whether it is still Freedom is the question the
business, not counsel, has to answer.

## 9. Questions for counsel, revised

Replacing `LISTING-RULES.md` §8. Ordered by how much turns on the answer.

1. **Public-company requirement.** Confirm that s.95(1)(a) read with
   s.97(1)(d) requires every issuer whose shares are dealt in on the exchange
   to be a public company with SEC-registered securities, and that no
   non-composite or ATS registration relaxes it. If confirmed, what is the
   lightest compliant path for a converting SME — s.104 exemption certificate,
   restricted invitation under s.107(2), or a registered short-form prospectus?
2. **The settlement registration.** Given the s.357 definitions of securities
   settlement system and clearing and settlement company, does exchange
   registration cover an exchange-operated settlement layer for its own trades
   (route C in §5), or is a CSC registration or CSD participation (route A/B)
   required? Is there an SEC no-action or interpretive precedent?
3. **Is the buyback a distribution?** Does an allocation of an issuer's treasury
   shares to a cardholder as a consequence of a payment constitute an invitation
   to acquire securities by the issuer (s.98 deems an issuer to have made the
   invitation where it allots with a view to public distribution)? Does the
   treasury release require one programme approval or per-release approval?
4. **Capital timing and transitional relief.** Does the ₦5bn for a non-composite
   exchange apply at first registration for a new applicant, or can a new
   registrant apply for a transitional arrangement under Circular 26-1 §6?
5. **Tax character.** Are shares received for tapping a benefit in kind
   (PITA), an acquisition at nil cost (CGT on the full proceeds), or an
   acquisition at the reference price? The per-lot cost basis in `share/`
   assumes the last.
6. **Self-sponsorship.** May the operator of a non-composite exchange sponsor
   listings on its own venue and purchase those shares through its own buyback,
   and what separation does the SEC require (s.28(3)(f) board representation;
   related-party rules)?
7. **Nominee versus custodian.** Does holding cardholders' shares in an omnibus
   nominee account make Freedom a "custodian" (bailee of assets, s.357) with the
   ₦50bn non-bank custodian requirement, or does appointing a bank custodian
   with Freedom as nominee avoid it?
8. **Investor protection fund minimum.** What minimum has the SEC set by
   regulation under s.208(1) for a non-composite exchange at establishment?

## 10. Sources

Primary:

- Investments and Securities Act 2025 (Act No. 2 of 2025) — gazetted text,
  [sec.gov.ng](https://sec.gov.ng/documents/1319/Investments_and_Securities_Act_2025_x9rSXtI.pdf).
  Sections cited: 26–37 (exchanges), 41–44 (FMIs), 45–58 (insolvency and
  settlement finality), 86–88 (registration of securities, public company
  reporting), 95–98 (invitations to the public), 102–108 (prospectus), 121–125
  (conduct), 198–216 (investor protection fund), 357 (interpretation).
- SEC Circular No. 26-1, *Revised Minimum Capital for Regulated Capital Market
  Entities*, 16 January 2026 —
  [sec.gov.ng](https://sec.gov.ng/documents/1427/CIRCULAR_Number_26-1._Minimum_Capital_Requirements.pdf).
- SEC registration checklists: [securities exchange](https://sec.gov.ng/about/resources/checklists/individual-registration-requirements-for-each-cmo/securities-exchange-registration-requirements/),
  [registrar](https://sec.gov.ng/about/resources/checklists/individual-registration-requirements-for-each-cmo/registrar-registration-requirements/).
  Note these still show the 2015 capital figures; Circular 26-1 supersedes them.
- CBN, *Transaction Limits on Contactless Payments*, circular of 27 June 2023,
  and *Guidelines for Contactless Payments in Nigeria* (2023).

Secondary (used where the primary text could not be fetched):

- [Alliance Law Firm — analysis of Circular 26-1](https://alliancelawfirm.ng/an-analysis-of-revised-minimum-capital-requirements-for-capital-market-operators-sec-circular-no-26-1-insights-for-market-participants/)
- [THISDAY — SEC raises minimum capital, June 2027 deadline](https://www.thisdaylive.com/2026/01/17/sec-raises-minimum-capital-requirements-for-market-operators-fintechs-others-sets-june-2027-deadline/)
- [Daily Trust — CBN issues new directives on bank charges (Guide to Charges 2026)](https://dailytrust.com/cbn-issues-new-directives-on-bank-charges/);
  [Leadership — MSC 0.5% capped at ₦10,000](https://leadership.ng/cbn-scraps-card-maintenance-fees-waives-charges-for-transfers-of-n5000-below/)
- [Mondaq — requirements for a PSP licence](https://www.mondaq.com/nigeria/financial-services/1372606/requirements-for-payment-service-provider-license-in-nigeria);
  [LawPavilion — CBN licence categories](https://lawpavilion.com/blog/highlights-of-new-licence-requirements-for-payments-system-by-cbn/)
- [Pavestones — CBN contactless guidelines](https://pavestoneslegal.com/nigerias-payment-system-central-bank-of-nigeria-guidelines-on-contactless-payments/);
  [BusinessDay — contactless limits](https://businessday.ng/news/legal-business/article/contactless-payments-in-nigeria-cbn-introduces-new-payment-limits/)
- [Financial Nigeria — ISA 2025 and crowdfunding](https://www.financialnigeria.com/how-the-isa-2025-reshapes-nigeria-s-crowdfunding-regulation-feature-597.html);
  [TechCabal — SEC crowdfunding rules](https://techcabal.com/2021/02/15/crowdfunding-regulations/)
- [Lexology / Alliance — ISA 2025 highlights](https://alliancelawfirm.ng/major-highlights-of-the-investments-and-securities-act-2025-a-new-dawn-for-nigerias-capital-market/)

---

*Cross-references: `docs/LISTING-RULES.md` §2 for admission (needs the §4
gate); `docs/SECURITY.md` §9 for the shorter summary this replaces;
`docs/CONTINUITY.md` §3 for what route B in §5 does to the single-datastore
assumption; `internal/fee/schedule.go` for the MSC cap in §6.1.*
