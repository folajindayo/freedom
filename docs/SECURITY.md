# Ọja — Security Architecture

Ọja is an issuer, a scheme, and an acquirer at once. That concentration is the
commercial advantage — it is what lets 80% of the fee pool be redirected into
equity — and it is also the security problem: there is no second party whose
controls catch our mistakes. Every compensating control a four-party network
gets for free has to be built deliberately here.

This document states what is defended, what is deliberately not, and where the
line currently sits. It is written to be argued with.

---

## 1. Threat model

| Adversary | Capability | Primary defence |
|---|---|---|
| Opportunist with an NFC phone | Read a tag in a queue, write a blank tag | Rolling token + read counter; detection, not prevention (§2) |
| Equipped cloner (Proxmark) | Emulate UID and counter together | **Not defended on NTAG215.** Priced as fraud loss; closed by NTAG424 (§2) |
| Malicious merchant | Forge taps, inflate tickets, collude on listings | Terminal attestation, velocity, trailing-VWAP cap, daily release cap (§6) |
| Compromised merchant phone | Root, hooked runtime, replayed captures | Per-terminal keys, attestation, capture ≤ authorised (§5) |
| Insider with database access | Read PANs, rewrite history, mint equity | PAN vault outside the DB, append-only ledger, closed transactions (§3, §4) |
| Insider with host access | Read the card master key from memory | **Not defended in the software keystore.** HSM before go-live (§3) |
| Network attacker | Intercept or replay scheme messages | mTLS between participants, per-message idempotency keys |
| Colluding listed company | Wash-trade its own price, extract fee pool | Price-taker design, trailing band, release cap, authorised ceiling (§6) |
| Fraudulent cardholder | Tap, take equity, charge back | 120-day transferability lock on every buyback lot (§6) |

---

## 2. Card credentials

### NTAG215 (pilot) — detection, not authentication

NTAG215 has **no cryptography**. No key, no challenge-response, nothing it can
compute. Its UID is readable and its user memory can be dumped, so it cannot
prove it is itself. Anyone who says otherwise is selling something.

What it does have is a **24-bit one-way read counter** that increments on the
first valid READ after entering an RF field, **32-bit password protection**
(PWD/PACK) on the write area, and 504 bytes of user memory. The pilot credential
uses all three:

1. **Rolling token** — 16 CSPRNG bytes written into user memory. The PoS reads
   it, the issuer burns it, the PoS writes the next one back.
2. **Counter monotonicity** — a counter that regresses means two tags answer to
   one UID. That is physically impossible for a single tag.
3. **PWD-locked write area** — a casual attacker cannot rewrite the token.

**The two-token window.** A single expected token is wrong in practice: the
customer lifts the card the moment the terminal beeps, the write-back never
lands, and the next genuine tap looks like a clone. So the previous token stays
live — *but only above the counter at which it was superseded*. A genuine retry
after a failed write always reports a higher counter; a duplicate of one tap
reports the same. Without that gate the fallback is itself a replay window, and
eight concurrent taps on one token were approved twice before it existed.
(`TestConcurrentTapsApproveExactlyOnce`)

**What is not defended.** A reader held near a card can lift the live token and
write a blank tag. If that clone taps before the real card does, it is approved,
and the real card is the one that gets frozen. A Proxmark can emulate UID and
counter together. **This is a priced fraud loss, not a solved problem**, and it
is why the NTAG215 path ships:

- online-only, no offline approvals, no floor limit
- per-transaction and daily caps (`cards.per_txn_cap_kobo`, `daily_cap_kobo`)
- a closed pilot cohort
- freeze-on-detection for both credential *and* card — two tags carry one
  identity and the issuer cannot tell which is which

The limit is asserted by a test (`TestNTAG215LiftedTokenIsAcceptedOnce`) rather
than described in a comment, so a future change that appears to fix it without
moving to NTAG424 reads as wrong.

### NTAG424 DNA (production) — a real credential

AES-128 CMAC over a monotonic counter, per NXP **AN12196**. The tag mirrors
encrypted PICCData and a CMAC into its own NDEF URL on every read. Verification:

```
PICCData  ← AES-128-CBC decrypt (SDMMetaReadKey, IV=0)
          → 0xC7 ‖ UID(7) ‖ SDMReadCtr(3, little-endian) ‖ pad
SV2       = 3C C3 00 01 00 80 ‖ UID ‖ SDMReadCtr ‖ zero-pad to 16
K_session = CMAC-AES128(SDMFileReadKey, SV2)
MAC       = truncate_NXP(CMAC(K_session, macInput))     # odd bytes 1,3,…,15
```

Three details that bite implementers, each pinned by a test:

- **Truncation keeps the odd-indexed bytes.** Taking the first eight verifies
  nothing and rejects every genuine tag.
- **The counter is little-endian.** Getting it backwards only fails past 255
  taps — well after a pilot would have signed off.
- **The UID must be taken from the decrypted PICCData, never from the reader.**
  `Parse` deliberately leaves `Presentment.UID` empty so the lookup hint cannot
  be mistaken for a verified identity.

Replay is refused on `counter <= last_seen`, and the counter advances by a
*conditional* UPDATE — read-then-write lets two concurrent taps accept the same
value.

---

## 3. Key management

Per-card keys are diversified from a master: `CMAC(master, 0x01 ‖ label ‖ 0x00 ‖ cardID)`,
with distinct labels so a card's meta-read and file-read keys are
cryptographically unrelated.

> This is AN10922-**shaped** but is Ọja's own scheme. That is safe only because
> Ọja personalises its own tags and verifies them here. Introducing an NXP SAM
> or NXP personalisation tooling means adopting AN10922 bit-for-bit — a
> near-miss produces tags this code cannot verify.

**`SoftwareKeyStore` is a real implementation, not a stub — and it is not an
HSM.** The master key is readable by anything that can read process memory or
the environment, so a host compromise is a compromise of every card ever issued.
There is no per-card blast radius and no key ceremony.

It refuses to start without a key: no default, no generated fallback. A scheme
that boots with a random key it did not persist has issued cards it can never
verify again; one that boots with a well-known default has issued cards anyone
can clone. Refusing is the only correct behaviour.

**Before any card is issued outside a controlled pilot:**

- master key moves to a FIPS 140-2 Level 3 HSM under dual control, and the
  software master is destroyed rather than migrated
- keys at rest wrapped in **ANSI X9.143** key blocks (which superseded TR-31),
  binding key attributes to key material — PCI PIN requires this and a file of
  raw hex does not meet the bar
- documented key ceremony: split custody, recorded, witnessed
- if PIN entry is ever added, **ANSI X9.24 DUKPT** per-terminal keys and the
  PCI PIN requirements that come with them

---

## 4. PAN handling and PCI DSS scope

Ọja is the issuer, so it holds PANs, and that puts it squarely in **PCI DSS
4.0.1** scope. Scope is contained by structure rather than by policy:

| Where | What |
|---|---|
| `card_pan_vault` (separate, encrypted under an external KEK) | the full PAN |
| `cards.pan_token` | keyed HMAC under a pepper held **outside** the database |
| `cards.pan_last4`, `pan_bin` | everything any other table or screen displays |

The token is a **keyed** hash, not a bare one. PCI DSS 4.0.1 calls for a keyed
cryptographic hash with associated key management precisely because the PAN
space is small enough to brute-force a plain hash. Everything else in the schema
joins on `card_id`.

PAN and CVV are never logged, never returned by an API that is not explicitly a
reveal endpoint, and never written to the ledger.

---

## 5. Merchant acceptance (softPOS)

The merchant's own Android phone is the terminal, which is exactly the scenario
**PCI MPoC** exists for — the standard that consolidated SPoC (PIN entry on
COTS) and CPoC (contactless on COTS). MPoC certification is a go-live
requirement, not a pilot one, but the data model carries its shape now:
per-terminal registration, attestation verdicts with their age, and a tamper
signal.

**Play Integrity fails open, deliberately.** Google has outages. An acceptance
fleet that fails closed on a third party's availability is a network-wide outage
triggered by someone else's incident. The verdict and its age are recorded
(`terminals.attestation_verdict`, `attested_at`) and the risk engine decides;
the authorisation path does not hard-fail on a missing token.

**Capture is bounded by authorisation.** `acquirer.Capture` refuses to present
more than the outstanding amount. Trusting the terminal's arithmetic is how a
merchant accidentally bills a customer twice.

---

## 6. Integrity of the buyback and the exchange

This is where a novel product creates novel attacks, and it deserves more
scrutiny than the payments path, which at least has fifty years of prior art.

### The buyback must never price itself

**The buyback is a price taker.** It buys from treasury at the price a call
auction of genuine orders discovered, and it is never itself an input to that
auction. If it were, the network's own demand would set the price the network
pays — the more it spent, the more each share would cost, and fee income would
drain into company treasuries by construction.

### Wash trading

A listing controlling two accounts can trade with itself on a thin book and
double its own reference price, then sell the network shares at 2×. Three
defences, all present:

1. **Trailing-VWAP cap** — the realised price is capped at the volume-weighted
   average of the trailing sessions, so one manipulated session cannot be cashed
   in. Sessions with no volume contribute nothing, so a run of carry-forward
   days cannot anchor the cap at a price nobody traded at.
2. **Daily release cap** (`treasury_pools.daily_release_units`) — a hard
   per-instrument bound on units released per session, reserved atomically.
   This is the binding defence on a new listing, which has no trailing history.
3. **Authorised ceiling** (`instruments.shares_authorised_units`) — every
   buyback is issuance, and issuance stops at what the company authorised.

Still open: **self-trade prevention** — excluding orders from accounts related
to the issuer's cap table from the reference calculation. Listed as work.

### Dilution is continuous and must be disclosed

Every tap sells treasury shares, which is continuous issuance: **existing
shareholders dilute on every transaction.** `cap_table_events` is an append-only
log rather than a running total, so dilution can always be reconstructed. The
disclosure obligation that follows is a product and legal question, not a
technical one, but the data to support it exists from the first row.

### Chargeback-funded equity

A fraudster taps, receives equity, sells it, and charges the tap back. Every
buyback lot therefore carries `transferable_from` = business date + 120 days.
The shares exist and are visible, and they cannot be moved until the chargeback
window closes.

### Eligibility is checked at the last possible moment

`equity_allocation_permitted()` requires KYC tier ≥ 1, a verified BVN, an
accepted disclosure, and an active account. It is checked in the allocator
itself, not trusted from upstream, because that is the last point before shares
exist in someone's name. A funded intent for an ineligible holder is parked and
stays claimable — never dropped, never swept.

---

## 7. Ledger and settlement integrity

- **Per-asset zero-sum**, enforced by a deferred constraint trigger. Naira and
  equity balance independently; summing them would be arithmetic on unlike things.
- **Settlement finality in the schema.** Entries may only be added by the
  database transaction that opened their header (`assert_ledger_tx_open`). Once
  committed, a set is closed forever, so a correction is a new event with its
  own audit trail — never an edit to a batch already reported to a participant.
  Without this, one appended entry would bypass the balance trigger entirely.
- **Idempotency keys are unique in the database**, not remembered by a job. Every
  switch-driven post has a natural key: `acquirer|business_date|terminal|stan`.
- **Business date is assigned once** at the switch off a 20:00 Lagos cutover and
  stamped. Never re-derived — a value that is recomputed is a value that can change.
- **Fee schedules are version-pinned at authorisation.** Clearing prices with the
  version the cardholder tapped under, not whichever is live when the batch runs.
  A mid-day rate change otherwise makes clearing disagree with the merchant's
  quote and reconciliation never balances again.
- **Net debit caps** (`participants.net_debit_cap_kobo`) bound what a member may
  owe before settlement. Without them, one member's failure is every member's loss.

### Participant isolation

Two Postgres roles, and they are a deployment decision that must be made before
any participant data exists:

| Role | RLS | Used by |
|---|---|---|
| `oja_participant` | enforced | acquirer API, member portal |
| `oja_scheme` | `BYPASSRLS` | switch, clearing, auction engine |

`FORCE ROW LEVEL SECURITY` is worth nothing if every binary connects as the same
role. Note that **`ledger_entries` is not participant-readable at all** — a
deferred balance trigger running under a policy that hides half a transaction
would raise "unbalanced" on perfectly good data. Participants see
`settlement_positions` and a curated transaction view.

---

## 8. Information disclosure

Decline codes follow ISO 8583 DE39 so an integrating member bank finds codes it
already knows. Internally the verifier distinguishes nine reasons; the terminal
is told far fewer. **Cryptographic failures are reported as a plain decline**:
telling a prober which check failed hands them an oracle. A bad CMAC is the one
exception in the other direction — it does *not* freeze the card, because it is
far more often a personalisation or key-rotation mistake, and freezing on it
turns one mis-keyed batch into a mass lockout.

---

## 9. Regulatory surface

Two live surfaces beyond the bank licence:

- **CBN** — switching and PoS acceptance. Also sets the merchant service charge
  (the 0.5%/₦1,000-cap shape in `fee.SchemeV1`); **verify the current figure
  against the CBN Guide to Charges before go-live.** It is a regulated number,
  not a commercial choice, and it moves.
- **SEC Nigeria** — operating an exchange and acting as a registrar. The buyback
  distributes securities, which makes disclosure, suitability, lock-ups and
  per-lot cost basis data requirements rather than features.

Build-in requirements already present: append-only audit trail, BVN/NIN capture,
disclosure acceptance recorded with its version, holding lots with cost basis
(which cannot be reconstructed later — capital gains reporting will need it),
and eligibility enforced as a database function.

---

## 10. Known gaps

Stated plainly, because a security document that lists only what is defended is
marketing.

1. **NTAG215 is not authentication.** Bounded by caps and a closed cohort.
2. **No HSM yet.** Software keystore; host compromise is total compromise.
3. **PAN vault is specified but not implemented.** The schema reserves its shape.
4. **No self-trade prevention** on the reference price calculation.
5. **No fraud/velocity engine** beyond fixed per-card caps.
6. **Chargeback unwind is not implemented** — the transferability lock that makes
   it possible exists; the unwind path itself does not.
7. **Dispute clocks** (chargeback, representment, pre-arbitration) are not built.
8. **No certification harness** for onboarding member banks.
9. **MPoC certification** not started.

---

## References

- [PCI MPoC — Mobile Payments on COTS](https://www.pcisecuritystandards.org/standards/mobile-payments-on-cots-mpoc/) (guidance published Nov 2025)
- [PCI CPoC — Contactless Payments on COTS](https://www.pcisecuritystandards.org/standards/contactless-payments-on-cots-cpoc/)
- [PCI DSS 4.0.1 encryption and tokenization requirements](https://www.thoropass.com/blog/pci-dss-encryption-requirements)
- [ANSI X9.143 / TR-31 key blocks](https://www.ibm.com/docs/en/zos/2.5.0?topic=cryptography-ansi-x9143-tr-31-key-block-support)
- [PCI PIN key block requirements](https://www.cryptomathic.com/news-events/blog/pci-pin-requirements-for-key-blocks-in-the-payment-card-industry-faqs)
- [NXP AN12196 — NTAG 424 DNA features and hints](https://www.nxp.com/docs/en/application-note/AN12196.pdf)
- [NTAG 424 DNA SDM reference implementation](https://github.com/AndroidCrypto/Ntag424SdmFeature)
- [RFC 4493 — AES-CMAC](https://www.rfc-editor.org/rfc/rfc4493) (test vectors pinned in `cmac_test.go`)
