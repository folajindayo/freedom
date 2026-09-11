# Freedom — Business Continuity

*v0.1 draft · reviewed annually · tested annually*

A venue that cannot say what happens when it breaks is not a venue. This
document exists to be tested, not filed: an untested continuity plan is a
statement of hope.

> **Status: draft.** The recovery objectives below are commitments the business
> has not yet made. They are written as the numbers a market of this kind should
> be able to defend, so that agreeing or arguing with them is possible.

---

## 1. What must not be lost

Ranked, because in a real incident you cannot protect everything at once.

1. **The ledger.** Every balance, every posting. It is the authoritative record
   of who owns what and who is owed what, and it is the only thing here that
   cannot be reconstructed from anything else.
2. **The order event log.** Sessions replay from it. Without it a settled
   auction cannot be re-derived and no price can be defended afterwards.
3. **Card credentials and keys.** A lost master key means every tag in the field
   stops verifying, permanently.
4. **The share register and holding lots.** Cost basis in particular cannot be
   reconstructed later, and capital gains reporting depends on it.

Market data, indices and statements are all derived. They can be rebuilt.

## 2. Recovery objectives ⚠

| | Objective | Why |
|---|---|---|
| **RPO** — data loss | **0 for the ledger** | A committed posting that vanishes means somebody's money vanished. Nothing else is acceptable. |
| **RTO** — card authorisation | **15 minutes** | Cardholders are standing at counters. A rail that is down is a card that does not work. |
| **RTO** — exchange trading | **1 session** | A missed auction is a carried-forward reference, which the system already handles honestly. Trading can wait; payments cannot. |
| **RTO** — settlement and buyback | **1 business day** | Clearing is T+1 by design; a day of slack already exists. |

The asymmetry is the point. The card rail and the exchange fail differently and
should be restored differently, and pretending otherwise leads to protecting the
wrong one first.

## 3. Architecture

Postgres is the only stateful component, which is a continuity decision as much
as an operational one: there is exactly one thing to replicate, back up and
restore, and no possibility of two datastores disagreeing after a partial
recovery.

- **Synchronous replica** in a second availability zone. Synchronous, not
  asynchronous — an RPO of zero for the ledger is not achievable otherwise.
- **Point-in-time recovery** with continuous WAL archiving, 35-day retention. ⚠
- **Daily logical backup**, restored into a scratch database and verified by
  running the test suite against it. A backup nobody has restored is a file, not
  a backup.
- Stateless services in two zones behind a health check.

## 4. Degraded modes

What the system does when a dependency is gone, chosen so that the failure is
never silent.

| Dependency | Degraded behaviour |
|---|---|
| Play Integrity (device attestation) | **Fails open**, verdict recorded with its age, risk engine decides. An acceptance fleet that fails closed on a third party's availability is a network-wide outage caused by someone else's incident. |
| NIBSS / cash rail | Deposits and withdrawals queue as pending instructions. Trading and the buyback are unaffected — they never touch the external rail. |
| Price reference stale | The buyback **escrows** rather than buying at a price from weeks ago. Already implemented as `stale_reference`. |
| Clock drift beyond tolerance | Recorded, and surfaced. A venue that cannot sequence events should not pretend to. |
| Replica lost | Continue on the primary, raise an incident. Running without a replica is a decision somebody should have to take knowingly. |
| **Primary lost** | Promote the replica. The last committed transaction survives; anything in flight did not commit and did not happen. |

## 5. What a real incident looks like

A worked example, because a plan that has never been walked through is a plan
nobody can follow at 3am.

**The primary database fails mid-auction, after freeze and before settlement.**

1. The transaction did not commit, so the session is still `frozen`. No fills
   exist, no reservations were consumed, no price was published.
2. Promote the replica. Point services at it.
3. Re-run `RunToSettlement` for that session. The book is unchanged, `Cross` is
   pure, and the `book_hash` recorded at freeze proves it — so the same price
   comes out.
4. Settlement posts under its existing idempotency key, so a partially-applied
   retry cannot double-post.
5. Run reconciliation for the business date before reopening.

The reason this works is that every stage is either atomic or keyed. That is not
luck; it is what the ledger's idempotency keys and the engine's purity are for,
and this scenario is the reason they were worth the effort.

## 6. Testing

| Test | Frequency | Pass condition |
|---|---|---|
| Restore a backup into a scratch database | Weekly | The full test suite passes against the restored copy |
| Replica promotion | Quarterly | RTO met, zero ledger rows lost |
| Session replay from `order_events` | Continuous, in CI | `result_hash` re-derives |
| Full failover rehearsal | Annually | Documented, timed, witnessed |

The replay test is the one that runs on every commit, and it is the one that
matters most: it proves that the thing recovery depends on actually works, every
single day, rather than once a year in a drill.

## 7. Not yet built

Stated plainly, because a continuity document that lists only what is handled is
worse than none.

1. No second availability zone. Everything currently runs in one place.
2. No verified restore process — backups exist, restoration has never been
   rehearsed.
3. No incident runbook beyond §5, and no on-call rota.
4. No key ceremony or HSM, so §1's third item is currently protected by a file.
   See `docs/SECURITY.md` §3.
5. No communications plan. Members and cardholders would find out by trying.

---

*Cross-references: `docs/SECURITY.md` §10 for the security gap list;
`internal/recon/` for the reconciliation run in §5 step 5.*
