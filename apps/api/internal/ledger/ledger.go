// Package ledger is the only place in Freedom where value moves.
//
// Every movement is a set of signed entries sharing a transaction id, and the
// entries must sum to exactly zero WITHIN EACH ASSET. The database enforces
// that with a deferred constraint trigger, so an unbalanced write cannot be
// committed even by a bug in this package.
//
// # Two assets, one engine
//
// Naira and equity are both integer minor units, so they ride the same rails. A
// buyback is one transaction with two balanced legs:
//
//	NGN:        scheme buyback_pool  −₦5.00     company treasury_cash  +₦5.00
//	EQ:MAMAPUT: company treasury     −0.125     cardholder stock_wallet +0.125
//
// Summing those together would be meaningless, which is why the invariant is
// per-asset and why the Go API takes amounts as a typed Amount rather than a
// bare int64.
//
// # Idempotency
//
// Post takes an idempotency key and the database holds it unique. A switch
// replays constantly — a retried capture, a re-run clearing batch, a webhook
// delivered twice — and every such path has a natural key available. Callers
// get ErrAlreadyPosted and the id of the transaction that won, not a duplicate.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ErrAlreadyPosted is returned when a transaction with the same idempotency key
// has already been committed. It is an expected outcome on a retry path, not a
// failure: callers should read the existing transaction and carry on.
var ErrAlreadyPosted = errors.New("ledger: transaction already posted")

// The asset id of naira. Equity asset ids are "EQ:" + symbol.
const AssetNGN = "NGN"

// EquityAsset returns the asset id for a listed instrument's symbol.
func EquityAsset(symbol string) string { return "EQ:" + symbol }

// Account kinds. The sign convention throughout is that a positive balance is
// value the owner has.
const (
	KindAvailable          = "available"
	KindHold               = "hold"
	KindStockWallet        = "stock_wallet"
	KindBuybackResidual    = "buyback_residual"
	KindMerchantReceivable = "merchant_receivable"
	KindMerchantSettled    = "merchant_settled"
	KindMerchantReserve    = "merchant_reserve"
	KindSettlement         = "settlement"
	KindCollateral         = "collateral"
	KindInterchangeIncome  = "interchange_income"
	KindTreasury           = "treasury"
	KindTreasuryCash       = "treasury_cash"
	KindRevenue            = "revenue"
	KindBuybackPool        = "buyback_pool"
	KindPromoBudget        = "promo_budget"
	KindSTIPLiability      = "stip_liability"
	KindLossReserve        = "loss_reserve"
	KindFloat              = "float"
	KindSuspense           = "suspense"
	KindExternal           = "external"

	// Exchange. Reservations are deliberately not KindHold: both mean "set
	// aside", but a card authorisation hold and an order reservation are
	// released by different jobs with different retry semantics, and sharing
	// one kind means an order release eventually frees card value.
	KindOrderCashReserve  = "order_cash_reserve"
	KindOrderShareReserve = "order_share_reserve"
	KindExchangeFeeIncome = "exchange_fee_income"
	KindTaxWithheld       = "tax_withheld"
	// KindBuybackLossReserve absorbs the shortfall when equity clawed back
	// after a chargeback cannot be fully recovered.
	KindBuybackLossReserve = "buyback_loss_reserve"
)

// Owner types.
const (
	OwnerCardholder  = "cardholder"
	OwnerMerchant    = "merchant"
	OwnerParticipant = "participant"
	OwnerCompany     = "company"
	OwnerScheme      = "scheme"
)

// Amount is a signed quantity in an asset's minor unit. Construct it with NGN
// or Equity rather than by hand, so the asset id and the scale always agree.
type Amount struct {
	Asset string
	Raw   int64
}

// NGN builds a naira amount.
func NGN(k money.Kobo) Amount { return Amount{Asset: AssetNGN, Raw: int64(k)} }

// Equity builds an amount of one instrument's shares.
func Equity(symbol string, u share.Units) Amount {
	return Amount{Asset: EquityAsset(symbol), Raw: int64(u)}
}

// Entry is one leg of a ledger transaction.
type Entry struct {
	AccountID uuid.UUID
	Amount    Amount
	Reason    string
}

// Tx describes an economic event: what happened, when it happened for scheme
// purposes, and the key that makes writing it twice a no-op.
type Tx struct {
	EventType      string
	BusinessDate   string // YYYY-MM-DD, assigned at the switch from the cutover clock
	IdempotencyKey string
	CorrelationID  *uuid.UUID
	Entries        []Entry
}

// AccountRef identifies an account by what it is rather than by id, so callers
// do not have to resolve one before they can post.
type AccountRef struct {
	OwnerType string
	OwnerID   *uuid.UUID
	Kind      string
	Asset     string
}

// Cardholder, Merchant, Company, Participant and Scheme build account
// references. They exist so that a miswired posting reads wrong at the call
// site rather than balancing silently into the wrong owner's account.
func Cardholder(id uuid.UUID, kind, asset string) AccountRef {
	return AccountRef{OwnerCardholder, &id, kind, asset}
}
func Merchant(id uuid.UUID, kind, asset string) AccountRef {
	return AccountRef{OwnerMerchant, &id, kind, asset}
}
func Company(id uuid.UUID, kind, asset string) AccountRef {
	return AccountRef{OwnerCompany, &id, kind, asset}
}
func Participant(id uuid.UUID, kind, asset string) AccountRef {
	return AccountRef{OwnerParticipant, &id, kind, asset}
}
func Scheme(kind, asset string) AccountRef {
	return AccountRef{OwnerScheme, nil, kind, asset}
}

// Resolve returns the account id for a reference, creating the account on first
// use. Accounts are cheap and creating them lazily keeps onboarding from having
// to predict every instrument a cardholder will ever hold.
func Resolve(ctx context.Context, q Querier, ref AccountRef) (uuid.UUID, error) {
	// Read first. The obvious spelling — INSERT ... ON CONFLICT DO UPDATE SET
	// kind = EXCLUDED.kind — writes a new row version on every call even when
	// nothing changed, taking a row lock and bumping xmin. An auction settling
	// ten thousand orders would do ten thousand no-op updates on the same few
	// hot accounts (scheme revenue, a company's treasury) and serialise the
	// whole session behind them, while bloating the table for the vacuum to
	// find later.
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM accounts
		 WHERE owner_type = $1 AND owner_id IS NOT DISTINCT FROM $2
		   AND kind = $3::account_kind AND asset_id = $4`,
		ref.OwnerType, ref.OwnerID, ref.Kind, ref.Asset).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("resolve %s/%s account: %w", ref.OwnerType, ref.Kind, err)
	}

	// First use of this account. DO NOTHING rather than DO UPDATE so a
	// concurrent creator does not turn into a lock wait, then re-read.
	err = q.QueryRow(ctx, `
		INSERT INTO accounts (owner_type, owner_id, kind, asset_id)
		VALUES ($1, $2, $3::account_kind, $4)
		ON CONFLICT (owner_type, owner_id, kind, asset_id) DO NOTHING
		RETURNING id`,
		ref.OwnerType, ref.OwnerID, ref.Kind, ref.Asset).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `
			SELECT id FROM accounts
			 WHERE owner_type = $1 AND owner_id IS NOT DISTINCT FROM $2
			   AND kind = $3::account_kind AND asset_id = $4`,
			ref.OwnerType, ref.OwnerID, ref.Kind, ref.Asset).Scan(&id)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve %s/%s account: %w", ref.OwnerType, ref.Kind, err)
	}
	return id, nil
}

// Post writes a balanced set of entries as one transaction.
//
// It refuses to issue the write at all if the entries do not balance, so the
// caller gets a clear error naming the asset rather than a deferred trigger
// failure at commit time with no context.
//
// The entries of one transaction must land inside a single database
// transaction, or the append-only trigger rejects the second one. When handed a
// bare pool, Post opens its own; when handed an existing pgx.Tx it joins the
// caller's, which is how a clearing batch posts thousands of events atomically.
func Post(ctx context.Context, q Querier, tx Tx) (uuid.UUID, error) {
	if tx.IdempotencyKey == "" {
		return uuid.Nil, errors.New("ledger: an idempotency key is required")
	}
	if tx.BusinessDate == "" {
		return uuid.Nil, errors.New("ledger: a business date is required")
	}
	if len(tx.Entries) < 2 {
		return uuid.Nil, fmt.Errorf("ledger: a movement needs at least two entries, got %d", len(tx.Entries))
	}

	sums := map[string]int64{}
	for _, e := range tx.Entries {
		if e.Amount.Raw == 0 {
			return uuid.Nil, fmt.Errorf("ledger: zero-amount entry (%s) is not a movement", e.Reason)
		}
		if e.Amount.Asset == "" {
			return uuid.Nil, fmt.Errorf("ledger: entry (%s) has no asset", e.Reason)
		}
		sums[e.Amount.Asset] += e.Amount.Raw
	}
	for asset, sum := range sums {
		if sum != 0 {
			return uuid.Nil, fmt.Errorf("ledger: unbalanced transaction, %s entries sum to %d", asset, sum)
		}
	}

	if pool, isPool := q.(*pgxpool.Pool); isPool {
		dbtx, err := pool.Begin(ctx)
		if err != nil {
			return uuid.Nil, err
		}
		defer dbtx.Rollback(ctx)

		id, err := insert(ctx, dbtx, tx)
		if err != nil {
			return id, err
		}
		if err := dbtx.Commit(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("ledger: commit: %w", err)
		}
		return id, nil
	}
	return insert(ctx, q, tx)
}

func insert(ctx context.Context, q Querier, tx Tx) (uuid.UUID, error) {
	// ON CONFLICT DO NOTHING rather than letting the unique violation raise.
	//
	// A raised error aborts the whole Postgres transaction, and Post is
	// routinely called inside a much larger one — a clearing batch, or an
	// authorisation that still has work to do afterwards. Letting the duplicate
	// surface as an error would poison every subsequent statement in that
	// transaction with "current transaction is aborted", turning an expected
	// retry into a total failure of the batch around it.
	var txID uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO ledger_tx (event_type, business_date, idempotency_key, correlation_id)
		VALUES ($1, $2::date, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		tx.EventType, tx.BusinessDate, tx.IdempotencyKey, tx.CorrelationID).Scan(&txID)

	if errors.Is(err, pgx.ErrNoRows) {
		// Already posted. Return the id of the transaction that won, so a
		// caller on a retry path can reference the original rather than having
		// to go looking for it.
		var existing uuid.UUID
		if err := q.QueryRow(ctx,
			`SELECT id FROM ledger_tx WHERE idempotency_key = $1`, tx.IdempotencyKey).Scan(&existing); err != nil {
			return uuid.Nil, fmt.Errorf("ledger: locating existing transaction %q: %w", tx.IdempotencyKey, err)
		}
		return existing, fmt.Errorf("%w (key %q)", ErrAlreadyPosted, tx.IdempotencyKey)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("ledger: open transaction: %w", err)
	}

	for _, e := range tx.Entries {
		if _, err := q.Exec(ctx, `
			INSERT INTO ledger_entries (tx_id, account_id, asset_id, amount, reason)
			VALUES ($1, $2, $3, $4, $5)`,
			txID, e.AccountID, e.Amount.Asset, e.Amount.Raw, e.Reason); err != nil {
			return uuid.Nil, fmt.Errorf("ledger: post %q: %w", e.Reason, err)
		}
	}
	return txID, nil
}

// Balance returns the current balance of one account.
func Balance(ctx context.Context, q Querier, ref AccountRef) (int64, error) {
	var bal int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(e.amount), 0)
		  FROM accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		 WHERE a.owner_type = $1
		   AND a.owner_id IS NOT DISTINCT FROM $2
		   AND a.kind = $3::account_kind
		   AND a.asset_id = $4`,
		ref.OwnerType, ref.OwnerID, ref.Kind, ref.Asset).Scan(&bal)
	return bal, err
}

// NairaBalance is Balance typed for naira accounts.
func NairaBalance(ctx context.Context, q Querier, ref AccountRef) (money.Kobo, error) {
	v, err := Balance(ctx, q, ref)
	return money.Kobo(v), err
}

// ShareBalance is Balance typed for equity accounts.
func ShareBalance(ctx context.Context, q Querier, ref AccountRef) (share.Units, error) {
	v, err := Balance(ctx, q, ref)
	return share.Units(v), err
}

// AssetImbalance returns, per asset, the sum of every entry in the ledger. All
// of them must be zero: value is only ever moved between accounts, never
// created. Call it from a test cleanup and from a nightly job — it is the one
// check that catches a whole class of bugs no unit test will.
func AssetImbalance(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, `
		SELECT asset_id, SUM(amount)::bigint FROM ledger_entries GROUP BY asset_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var asset string
		var sum int64
		if err := rows.Scan(&asset, &sum); err != nil {
			return nil, err
		}
		if sum != 0 {
			out[asset] = sum
		}
	}
	return out, rows.Err()
}
