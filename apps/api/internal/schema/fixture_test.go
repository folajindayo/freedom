package schema

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func randSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type fixture struct {
	participantID, cardholderID, cardID, credentialID string
	merchantID, terminalID                            string
}

// newFixture builds the minimum network needed to record a transaction: one
// participant wearing every role, one cardholder with a credentialed card, and
// one merchant with a terminal.
func newFixture(t *testing.T, p *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	var f fixture
	s := randSuffix()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(p.QueryRow(ctx, `
		INSERT INTO participants (code, legal_name, roles, status)
		VALUES ($1, 'Ọja Bank', ARRAY['issuer','acquirer','scheme'], 'active') RETURNING id`,
		"P"+s).Scan(&f.participantID))

	must(p.QueryRow(ctx, `
		INSERT INTO cardholders (phone, display_name, kyc_tier, bvn_verified_at,
		                         disclosure_accepted_at, disclosure_version)
		VALUES ($1,'Ada Okafor',1,now(),now(),'v1') RETURNING id`, "+234"+s).Scan(&f.cardholderID))

	must(p.QueryRow(ctx, `
		INSERT INTO cards (cardholder_id, issuer_id, product_code, pan_token, pan_last4, pan_bin, expires_on)
		VALUES ($1,$2,'classic',$3,'4242','50610000','2030-01-01') RETURNING id`,
		f.cardholderID, f.participantID, []byte(s)).Scan(&f.cardID))

	must(p.QueryRow(ctx, `
		INSERT INTO card_credentials (card_id, tech, tag_uid, token_current)
		VALUES ($1,'ntag215',$2,$3) RETURNING id`,
		f.cardID, []byte("uid"+s), []byte("tok"+s)).Scan(&f.credentialID))

	must(p.QueryRow(ctx, `
		INSERT INTO merchants (acquirer_id, legal_name, trading_name, mcc, kyb_status)
		VALUES ($1,'Mama Put Ltd','Mama Put','5812','verified') RETURNING id`,
		f.participantID).Scan(&f.merchantID))

	must(p.QueryRow(ctx, `
		INSERT INTO terminals (merchant_id, label) VALUES ($1,'Counter 1') RETURNING id`,
		f.merchantID).Scan(&f.terminalID))

	// Every authorisation pins a fee schedule version, so one must exist.
	_, err := p.Exec(ctx, `
		INSERT INTO fee_schedules (version, effective_from, definition)
		VALUES (1,'2026-01-01','{}'::jsonb) ON CONFLICT (version) DO NOTHING`)
	must(err)

	return f
}
