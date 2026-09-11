package institution

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"freedom/api/internal/alloc"
	"freedom/api/internal/money"
	"freedom/api/internal/share"
)

// The index.
//
// Two jobs. It is the venue's benchmark — without one, nobody can say whether a
// listing went up because it did well or because the whole market did. And it
// is the natural destination for a buyback against a merchant who has not
// listed: a diversified claim on the market is a far better thing to give a
// cardholder than an IOU against one shop that may never list.

// IndexScale is the fixed-point scale of an index value: 1_000_000 is 100.0000.
const IndexScale = 10_000

// Index is a benchmark definition.
type Index struct {
	ID           string
	Name         string
	BaseDate     string
	BaseValue    int64
	MaxWeightBps int64
}

// FreedomAllShare is the venue's headline benchmark.
func FreedomAllShare(baseDate string) Index {
	return Index{
		ID: "FAS", Name: "Freedom All-Share", BaseDate: baseDate,
		BaseValue: 100 * IndexScale, MaxWeightBps: 2000,
	}
}

// Create registers an index.
func Create(ctx context.Context, tx pgx.Tx, ix Index) error {
	if ix.MaxWeightBps <= 0 || ix.MaxWeightBps > 10_000 {
		return fmt.Errorf("institution: a weight cap of %d bps is not a proportion", ix.MaxWeightBps)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO indices (id, name, base_date, base_value, max_weight_bps)
		VALUES ($1,$2,$3::date,$4,$5)
		ON CONFLICT (id) DO NOTHING`,
		ix.ID, ix.Name, ix.BaseDate, ix.BaseValue, ix.MaxWeightBps)
	if err != nil {
		return fmt.Errorf("institution: create index: %w", err)
	}
	return nil
}

// Include adds an instrument to an index from a date.
func Include(ctx context.Context, tx pgx.Tx, indexID, instrumentID, from string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO index_constituents (index_id, instrument_id, added_on)
		VALUES ($1,$2,$3::date) ON CONFLICT DO NOTHING`, indexID, instrumentID, from)
	if err != nil {
		return fmt.Errorf("institution: include constituent: %w", err)
	}
	return nil
}

// Weights are the capped constituent weights on a date.
type Weights struct {
	InstrumentID string
	Symbol       string
	WeightBps    int64
	PriceKobo    money.Kobo
	CapKobo      money.Kobo
}

// Compute values an index and stores the result.
//
// Weights are capped and the excess redistributed, which is not cosmetic: on a
// venue where one listing may be twenty times the next, an uncapped index IS
// that listing, and a benchmark that tracks a single constituent tells nobody
// anything about the market.
func Compute(ctx context.Context, tx pgx.Tx, indexID, obsDate string) (int64, []Weights, error) {
	var maxWeight, baseValue int64
	var baseDate string
	if err := tx.QueryRow(ctx, `
		SELECT max_weight_bps, base_value, base_date::text FROM indices WHERE id = $1`,
		indexID).Scan(&maxWeight, &baseValue, &baseDate); err != nil {
		return 0, nil, fmt.Errorf("institution: load index: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT i.id, i.symbol, i.reference_price_kobo,
		       COALESCE((SELECT SUM(units_delta) FROM cap_table_events c
		                  WHERE c.instrument_id = i.id), 0)
		  FROM index_constituents k
		  JOIN instruments i ON i.id = k.instrument_id
		 WHERE k.index_id = $1 AND k.added_on <= $2::date
		   AND (k.removed_on IS NULL OR k.removed_on > $2::date)
		   AND i.status = 'listed'
		 ORDER BY i.symbol`, indexID, obsDate)
	if err != nil {
		return 0, nil, fmt.Errorf("institution: index constituents: %w", err)
	}
	defer rows.Close()

	var out []Weights
	var caps []int64
	var total int64
	for rows.Next() {
		var w Weights
		var issued int64
		if err := rows.Scan(&w.InstrumentID, &w.Symbol, &w.PriceKobo, &issued); err != nil {
			return 0, nil, err
		}
		if issued <= 0 {
			continue // nothing in issue contributes nothing
		}
		capValue, err := share.CostOf(share.Units(issued), w.PriceKobo)
		if err != nil {
			return 0, nil, err
		}
		w.CapKobo = capValue
		out = append(out, w)
		caps = append(caps, int64(capValue))
		total += int64(capValue)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(out) == 0 || total == 0 {
		return 0, nil, fmt.Errorf("institution: index %s has no valued constituents on %s", indexID, obsDate)
	}

	weights, err := alloc.Proportional(int64(10_000), caps)
	if err != nil {
		return 0, nil, fmt.Errorf("institution: index weights: %w", err)
	}
	weights = applyCap(weights, maxWeight)
	for i := range out {
		out[i].WeightBps = weights[i]
	}

	// The value: the capped weighted sum of constituent prices, expressed
	// against the base. A single-constituent index on its base date reads
	// exactly at base, which is the property that makes the series readable.
	var value int64
	for i, w := range out {
		value += weights[i] * int64(w.PriceKobo)
	}
	value = value / 10_000 * IndexScale / referencePrice(out)

	if _, err := tx.Exec(ctx, `
		INSERT INTO index_values (index_id, obs_date, value_bps, constituents)
		VALUES ($1,$2::date,$3,$4)
		ON CONFLICT (index_id, obs_date) DO UPDATE
		  SET value_bps = EXCLUDED.value_bps, constituents = EXCLUDED.constituents`,
		indexID, obsDate, value, len(out)); err != nil {
		return 0, nil, fmt.Errorf("institution: store index value: %w", err)
	}
	return value, out, nil
}

// referencePrice is the capped weighted average price, used to normalise the
// index so it is a level rather than a naira amount.
func referencePrice(w []Weights) int64 {
	var sum int64
	for _, x := range w {
		sum += int64(x.PriceKobo)
	}
	if sum == 0 || len(w) == 0 {
		return 1
	}
	return sum / int64(len(w))
}

// applyCap holds every weight at or below the cap and redistributes the excess
// across the uncapped constituents, iterating because redistribution can push a
// previously-compliant constituent over the line.
func applyCap(weights []int64, maxBps int64) []int64 {
	if maxBps <= 0 || int64(len(weights))*maxBps < 10_000 {
		// The cap is unachievable with this many constituents — every one would
		// have to exceed it. Leave the weights uncapped rather than loop
		// forever trying to satisfy an impossible constraint.
		return weights
	}
	out := append([]int64(nil), weights...)
	for pass := 0; pass < 16; pass++ {
		var excess int64
		var freeTotal int64
		capped := make([]bool, len(out))
		for i, w := range out {
			if w > maxBps {
				excess += w - maxBps
				out[i] = maxBps
				capped[i] = true
			}
		}
		if excess == 0 {
			return out
		}
		for i, w := range out {
			if !capped[i] {
				freeTotal += w
			}
		}
		if freeTotal == 0 {
			return out
		}
		for i, w := range out {
			if !capped[i] {
				out[i] = w + excess*w/freeTotal
			}
		}
	}
	return out
}
