package e2e

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"oja/api/internal/issuer"
	"oja/api/internal/ledger"
	"oja/api/internal/money"
	"oja/api/internal/scheme"
	"oja/api/internal/share"
	"oja/api/internal/tapcrypto"
)

// network is a complete, minimal Ọja: one bank wearing every role, one verified
// cardholder with a funded account and a credentialed card, one listed merchant
// with a treasury pool and a reference price, and a terminal to tap against.
type network struct {
	participantID uuid.UUID
	cardholderID  uuid.UUID
	cardID        uuid.UUID
	credentialID  uuid.UUID
	merchantID    uuid.UUID
	companyID     uuid.UUID
	terminalID    uuid.UUID
	instrumentID  string
	symbol        string

	tagUID []byte
	token  []byte

	auth *issuer.Authorizer
	stan int
}

func setup(t *testing.T, p *pgxpool.Pool) *network {
	t.Helper()
	ctx := context.Background()
	reset(t, p)
	s := randHex(5)
	n := &network{symbol: "SHOP" + strings.ToUpper(randHex(3))}
	n.instrumentID = ledger.EquityAsset(n.symbol)

	q := func(dest any, sql string, args ...any) {
		t.Helper()
		if err := p.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("setup: %v\n%s", err, sql)
		}
	}
	x := func(sql string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("setup: %v\n%s", err, sql)
		}
	}

	x(`INSERT INTO fee_schedules (version, effective_from, definition)
	   VALUES (1,'2026-01-01','{"name":"SchemeV1"}'::jsonb) ON CONFLICT (version) DO NOTHING`)

	q(&n.participantID, `
		INSERT INTO participants (code, legal_name, roles, status, net_debit_cap_kobo)
		VALUES ($1,'Ọja Bank Plc',ARRAY['issuer','acquirer','scheme'],'active',100000000000)
		RETURNING id`, "OJA"+s)

	// A cardholder who is verified and has accepted the risk disclosure —
	// without both, the buyback will not allocate equity to them.
	q(&n.cardholderID, `
		INSERT INTO cardholders (phone, display_name, kyc_tier, bvn_verified_at,
		                         disclosure_accepted_at, disclosure_version)
		VALUES ($1,'Ada Okafor',2,now(),now(),'risk-disclosure-v1') RETURNING id`, "+234"+s)

	q(&n.cardID, `
		INSERT INTO cards (cardholder_id, issuer_id, product_code, pan_token, pan_last4,
		                   pan_bin, expires_on, per_txn_cap_kobo, daily_cap_kobo)
		VALUES ($1,$2,'classic',$3,'4242','50610000','2030-12-31',5000000,20000000)
		RETURNING id`, n.cardholderID, n.participantID, []byte("pantok"+s))

	n.tagUID = mustHex(t, "04"+randHex(6))
	n.token = mustHex(t, randHex(tapcrypto.TokenBytes))
	q(&n.credentialID, `
		INSERT INTO card_credentials (card_id, tech, tag_uid, token_current, last_counter)
		VALUES ($1,'ntag215',$2,$3,0) RETURNING id`, n.cardID, n.tagUID, n.token)

	q(&n.companyID, `
		INSERT INTO companies (legal_name, rc_number) VALUES ('Mama Put Kitchens Ltd',$1)
		RETURNING id`, "RC"+s)

	q(&n.merchantID, `
		INSERT INTO merchants (acquirer_id, legal_name, trading_name, mcc, kyb_status, company_id)
		VALUES ($1,'Mama Put Kitchens Ltd','Mama Put','5812','verified',$2) RETURNING id`,
		n.participantID, n.companyID)

	q(&n.terminalID, `
		INSERT INTO terminals (merchant_id, label, attestation_verdict, attested_at)
		VALUES ($1,'Counter 1','trusted',now()) RETURNING id`, n.merchantID)

	// List the instrument: an asset, a symbol, an authorised ceiling, a treasury
	// pool with a daily release cap, and a reference price to buy at.
	x(`INSERT INTO assets (id,class,scale,label) VALUES ($1,'equity',8,$2)`, n.instrumentID, n.symbol)
	x(`INSERT INTO instruments (id, symbol, company_id, shares_authorised_units, status, listed_at)
	   VALUES ($1,$2,$3,$4,'listed',now())`,
		n.instrumentID, n.symbol, n.companyID, int64(share.Whole(10_000)))

	// Seed treasury with 1,000 shares from outside the system.
	mustTx(t, p, func(tx pgx.Tx) error {
		ext, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, n.instrumentID))
		if err != nil {
			return err
		}
		treas, err := ledger.Resolve(ctx, tx, ledger.Company(n.companyID, ledger.KindTreasury, n.instrumentID))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Tx{
			EventType:      "listing.treasury_seeded",
			BusinessDate:   sessionDate,
			IdempotencyKey: "listing|" + n.instrumentID,
			Entries: []ledger.Entry{
				{AccountID: ext, Amount: ledger.Equity(n.symbol, -share.Whole(1_000)), Reason: "listing.authorised"},
				{AccountID: treas, Amount: ledger.Equity(n.symbol, share.Whole(1_000)), Reason: "listing.treasury"},
			},
		})
		return err
	})

	var treasuryAcct uuid.UUID
	q(&treasuryAcct, `SELECT id FROM accounts WHERE owner_type='company' AND owner_id=$1
	                  AND kind='treasury' AND asset_id=$2`, n.companyID, n.instrumentID)
	x(`INSERT INTO treasury_pools (instrument_id, account_id, daily_release_units, release_date)
	   VALUES ($1,$2,$3,$4::date)`,
		n.instrumentID, treasuryAcct, int64(share.Whole(100)), sessionDate)

	// The reference price the buyback is a price-taker against.
	x(`INSERT INTO price_observations (instrument_id, obs_date, source, price_kobo, volume_units, content_hash)
	   VALUES ($1,$2::date,'auction',$3,0,'seed')`,
		n.instrumentID, sessionDate, int64(money.Naira(40)))

	// Fund the cardholder, so there is something to spend.
	mustTx(t, p, func(tx pgx.Tx) error {
		ext, err := ledger.Resolve(ctx, tx, ledger.Scheme(ledger.KindExternal, ledger.AssetNGN))
		if err != nil {
			return err
		}
		avail, err := ledger.Resolve(ctx, tx, ledger.Cardholder(n.cardholderID, ledger.KindAvailable, ledger.AssetNGN))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Tx{
			EventType:      "funding.received",
			BusinessDate:   sessionDate,
			IdempotencyKey: "funding|" + n.cardholderID.String(),
			Entries: []ledger.Entry{
				{AccountID: ext, Amount: ledger.NGN(-money.Naira(50_000)), Reason: "funding.bank_transfer"},
				{AccountID: avail, Amount: ledger.NGN(money.Naira(50_000)), Reason: "funding.credit"},
			},
		})
		return err
	})

	keys, err := tapcrypto.NewSoftwareKeyStore(mustHex(t, "0102030405060708090a0b0c0d0e0f10"))
	if err != nil {
		t.Fatal(err)
	}
	n.auth = issuer.New(keys)
	// Pin the clock so business dates in assertions are stable.
	at := time.Date(2026, 9, 11, 12, 0, 0, 0, scheme.Lagos)
	n.auth.Now = func() time.Time { return at }

	return n
}

// tap presents the card at the terminal, exactly as the PoS would: the UID, the
// rolling token currently on the tag, and the tag's read counter.
func (n *network) tap(ctx context.Context, tx pgx.Tx, amount money.Kobo) (issuer.Response, error) {
	n.stan++
	ctr := []byte{byte(n.stan), 0, 0}

	resp, err := n.auth.Authorize(ctx, tx, issuer.Request{
		TerminalID: n.terminalID,
		STAN:       fmt.Sprintf("%06d", n.stan),
		Amount:     amount,
		Tech:       tapcrypto.TechNTAG215,
		Tap: url.Values{
			"uid":   {hex.EncodeToString(n.tagUID)},
			"token": {hex.EncodeToString(n.token)},
			"ctr":   {hex.EncodeToString(ctr)},
		},
		At: time.Date(2026, 9, 11, 12, 0, 0, 0, scheme.Lagos),
	})
	if err != nil {
		return resp, err
	}
	// The PoS writes the next token back to the tag. Modelling this is the
	// point: a test that skipped it would never exercise rotation.
	if resp.WriteBack != nil {
		n.token = resp.WriteBack.Token
	}
	return resp, nil
}

func (n *network) balance(t *testing.T, p *pgxpool.Pool, kind string) money.Kobo {
	t.Helper()
	v, err := ledger.NairaBalance(context.Background(), p,
		ledger.Cardholder(n.cardholderID, kind, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (n *network) merchantBalance(t *testing.T, p *pgxpool.Pool) money.Kobo {
	t.Helper()
	v, err := ledger.NairaBalance(context.Background(), p,
		ledger.Merchant(n.merchantID, ledger.KindMerchantReceivable, ledger.AssetNGN))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (n *network) shares(t *testing.T, p *pgxpool.Pool) share.Units {
	t.Helper()
	v, err := ledger.ShareBalance(context.Background(), p,
		ledger.Cardholder(n.cardholderID, ledger.KindStockWallet, n.instrumentID))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (n *network) treasuryShares(t *testing.T, p *pgxpool.Pool) share.Units {
	t.Helper()
	v, err := ledger.ShareBalance(context.Background(), p,
		ledger.Company(n.companyID, ledger.KindTreasury, n.instrumentID))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// reset empties the network between end-to-end tests.
//
// Clearing deliberately processes every unbatched presentment on the network —
// that is what a clearing run is — so tests that share a database would
// otherwise pick up each other's traffic. The isolation belongs here rather
// than in a narrower clearing query that would not match production.
func reset(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	_, err := p.Exec(context.Background(), `
		TRUNCATE ledger_entries, ledger_tx, accounts, account_balance_snapshots,
		         participants, bin_ranges, cardholders, cards, card_credentials,
		         merchants, terminals, authorizations, presentments, clearing_batches,
		         companies, instruments, cap_table_events, treasury_pools,
		         auctions, orders, price_observations, data_quality_incidents,
		         holding_lots, buyback_batches, buyback_intents, fee_schedules
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	// Equity assets are per-test; naira is not.
	if _, err := p.Exec(context.Background(),
		`DELETE FROM assets WHERE class = 'equity'`); err != nil {
		t.Fatalf("reset assets: %v", err)
	}
}
