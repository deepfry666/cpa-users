package main

import (
	"database/sql"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

// Price is what one model costs, in the configured currency. Token prices are
// per one million tokens; PerRequest is charged once per successful request
// (image models are usually priced this way).
type Price struct {
	Model      string  `json:"model"`
	Input      float64 `json:"input"`
	Cached     float64 `json:"cached"`
	Output     float64 `json:"output"`
	PerRequest float64 `json:"per_request"`
	Note       string  `json:"note,omitempty"`
}

// Money is stored as integer micro-units so sums never drift.
const microsPerUnit = 1_000_000

func microsToMoney(m int64) float64 { return float64(m) / microsPerUnit }

func moneyToMicros(v float64) int64 { return int64(math.Round(v * microsPerUnit)) }

// maxMoney bounds prices and limits so a typo cannot overflow int64 micros.
const maxMoney = 1e9

func validMoney(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= maxMoney
}

// tokenUsage is what a price is applied to, in one canonical shape: Input is
// all input tokens including cache reads and writes, Cached is the cache-read
// part of Input, and Output includes reasoning. Rows are stored this way.
type tokenUsage struct {
	Input, Output, Reasoning, Cached, Total int64
	Successes                               int64
}

// normalizeUsage maps CPA's per-provider token report onto tokenUsage.
//
//   - OpenAI / Codex: input includes cached tokens, output includes reasoning,
//     total = input + output.
//   - Claude: input excludes cache reads and writes, which are reported
//     separately; output includes thinking; total = input + output + reads + writes.
//   - Gemini: input includes cached tokens, but thoughts are reported apart
//     from output; total = input + output + thoughts.
func normalizeUsage(d usageDetail) tokenUsage {
	u := tokenUsage{Input: d.InputTokens, Output: d.OutputTokens, Reasoning: d.ReasoningTokens, Cached: d.CachedTokens, Total: d.TotalTokens}
	cacheSide := d.CacheReadTokens + d.CacheCreationTokens
	switch {
	case cacheSide > 0 && d.TotalTokens == d.InputTokens+d.OutputTokens+cacheSide:
		u.Input = d.InputTokens + cacheSide
		u.Cached = d.CacheReadTokens
	case d.ReasoningTokens > 0 && d.TotalTokens == d.InputTokens+d.OutputTokens+d.ReasoningTokens:
		u.Output = d.OutputTokens + d.ReasoningTokens
	}
	if u.Cached > u.Input {
		u.Cached = u.Input
	}
	if u.Total == 0 {
		u.Total = u.Input + u.Output
	}
	return u
}

// costMicros prices one request (or one aggregated bucket) given in the
// canonical shape. Cache writes are charged at the input price.
func costMicros(p Price, u tokenUsage) int64 {
	cached := u.Cached
	if cached > u.Input {
		cached = u.Input
	}
	uncached := u.Input - cached
	cost := (float64(uncached)*p.Input + float64(cached)*p.Cached + float64(u.Output)*p.Output) / 1e6
	cost += float64(u.Successes) * p.PerRequest
	return moneyToMicros(cost)
}

// priceTable resolves a model name to its price: an exact entry wins, then
// the longest matching wildcard pattern.
type priceTable struct {
	exact    map[string]Price
	patterns []Price // longest pattern first
	rows     []Price // as stored, sorted by model
}

func newPriceTable(rows []Price) *priceTable {
	t := &priceTable{exact: map[string]Price{}, rows: rows}
	for _, p := range rows {
		key := strings.ToLower(strings.TrimSpace(p.Model))
		if strings.Contains(key, "*") {
			t.patterns = append(t.patterns, Price{Model: key, Input: p.Input, Cached: p.Cached, Output: p.Output, PerRequest: p.PerRequest})
		} else {
			t.exact[key] = p
		}
	}
	sort.SliceStable(t.patterns, func(i, j int) bool { return len(t.patterns[i].Model) > len(t.patterns[j].Model) })
	return t
}

func (t *priceTable) lookup(model string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	m := normalizeModel(model)
	if m == "" {
		return Price{}, false
	}
	if p, ok := t.exact[m]; ok {
		return p, true
	}
	for _, p := range t.patterns {
		if globMatch(p.Model, m) {
			return p, true
		}
	}
	return Price{}, false
}

// ---- storage ----

func (s *store) loadPrices() ([]Price, error) {
	rows, err := s.db.Query(`SELECT model, input, cached, output, per_request, note FROM prices ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Price{}
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.Model, &p.Input, &p.Cached, &p.Output, &p.PerRequest, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// savePrice upserts a price row; oldModel renames an existing row.
func (s *store) savePrice(p Price, oldModel string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if oldModel != "" && oldModel != p.Model {
		if _, err := tx.Exec(`DELETE FROM prices WHERE model = ?`, oldModel); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO prices (model, input, cached, output, per_request, note, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (model) DO UPDATE SET input = excluded.input, cached = excluded.cached, output = excluded.output,
			per_request = excluded.per_request, note = excluded.note, updated_at = excluded.updated_at`,
		p.Model, p.Input, p.Cached, p.Output, p.PerRequest, p.Note, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) deletePrice(model string) error {
	res, err := s.db.Exec(`DELETE FROM prices WHERE model = ?`, model)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// totalCost returns all-time spend per user, for lifetime limits.
func (s *store) totalCost() (map[int64]int64, error) {
	rows, err := s.db.Query(`SELECT user_id, SUM(cost_micros) FROM usage_hourly WHERE user_id != 0 GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var uid, sum int64
		if err := rows.Scan(&uid, &sum); err != nil {
			return nil, err
		}
		out[uid] = sum
	}
	return out, rows.Err()
}

// usedModels lists distinct models with usage since from.
func (s *store) usedModels(from time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT model FROM usage_hourly WHERE hour >= ? AND model != '' ORDER BY model`, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// recomputeCosts re-prices every stored bucket and request with the current
// table. It returns how many rows changed.
func (s *store) recomputeCosts(t *priceTable) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var changed int64

	type hourRow struct {
		hour, user, key                                      int64
		model                                                string
		reqs, failed, in, out, reasoning, cached, total, old int64
	}
	rows, err := tx.Query(`SELECT hour, user_id, key_id, model, requests, failed, input_tokens, output_tokens,
		reasoning_tokens, cached_tokens, total_tokens, cost_micros FROM usage_hourly`)
	if err != nil {
		return 0, err
	}
	var hours []hourRow
	for rows.Next() {
		var r hourRow
		if err := rows.Scan(&r.hour, &r.user, &r.key, &r.model, &r.reqs, &r.failed, &r.in, &r.out, &r.reasoning,
			&r.cached, &r.total, &r.old); err != nil {
			rows.Close()
			return 0, err
		}
		hours = append(hours, r)
	}
	rows.Close()
	for _, r := range hours {
		var cost int64
		if p, ok := t.lookup(r.model); ok {
			cost = costMicros(p, tokenUsage{Input: r.in, Output: r.out, Reasoning: r.reasoning, Cached: r.cached, Total: r.total, Successes: r.reqs - r.failed})
		}
		if cost == r.old {
			continue
		}
		if _, err := tx.Exec(`UPDATE usage_hourly SET cost_micros = ? WHERE hour = ? AND user_id = ? AND key_id = ? AND model = ?`,
			cost, r.hour, r.user, r.key, r.model); err != nil {
			return 0, err
		}
		changed++
	}

	type logRow struct {
		id                                     int64
		model                                  string
		failed                                 int
		in, out, reasoning, cached, total, old int64
	}
	rows, err = tx.Query(`SELECT id, model, failed, input_tokens, output_tokens, reasoning_tokens, cached_tokens,
		total_tokens, cost_micros FROM request_log`)
	if err != nil {
		return 0, err
	}
	var logs []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.id, &r.model, &r.failed, &r.in, &r.out, &r.reasoning, &r.cached, &r.total, &r.old); err != nil {
			rows.Close()
			return 0, err
		}
		logs = append(logs, r)
	}
	rows.Close()
	for _, r := range logs {
		var cost int64
		if p, ok := t.lookup(r.model); ok {
			succ := int64(1)
			if r.failed != 0 {
				succ = 0
			}
			cost = costMicros(p, tokenUsage{Input: r.in, Output: r.out, Reasoning: r.reasoning, Cached: r.cached, Total: r.total, Successes: succ})
		}
		if cost == r.old {
			continue
		}
		if _, err := tx.Exec(`UPDATE request_log SET cost_micros = ? WHERE id = ?`, cost, r.id); err != nil {
			return 0, err
		}
	}
	return changed, tx.Commit()
}

var errBadPrice = errors.New("价格必须是 0 到 10 亿之间的数字")
