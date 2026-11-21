package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const litellmFixture = `{
  "sample_spec": {"litellm_provider": "openai", "input_cost_per_token": 0},
  "gpt-6.1-sol": {"litellm_provider": "openai", "input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05, "cache_read_input_token_cost": 1e-07},
  "azure/gpt-6.1-sol": {"litellm_provider": "azure", "input_cost_per_token": 9e-06, "output_cost_per_token": 9e-05},
  "azure/gpt-6-luna": {"litellm_provider": "azure", "input_cost_per_token": 9e-06, "output_cost_per_token": 9e-05},
  "gpt-image-2": {"litellm_provider": "openai", "input_cost_per_token": 5e-06, "cache_read_input_token_cost": 1.25e-06, "output_cost_per_image_token": 3e-05},
  "anthropic/claude-x": {"litellm_provider": "anthropic", "input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05},
  "claude-x": {"litellm_provider": "anthropic", "input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05, "cache_read_input_token_cost": 3e-07},
  "deepseek-v4.1-flash": {"litellm_provider": "coralbricks", "input_cost_per_token": 1e-08, "output_cost_per_token": 1e-08}
}`

const openrouterFixture = `{"data": [
  {"id": "deepseek/deepseek-v4.1-flash"}, {"id": "deepseek/deepseek-v4.1-flash:free"},
  {"id": "someone/gpt-6-luna"}, {"id": "openai/gpt-6.1-sol"}, {"id": "deepseek/deepseek-v5"}
]}`

// deepseek-v4.1-flash is only served by third parties (cheaper than the
// vendor); deepseek-v5 has the vendor's own endpoint.
const endpointsThirdParty = `{"data": {"endpoints": [
  {"tag": "baidu/fp8", "pricing": {"prompt": "0.0000000498", "completion": "0.0000001992"}}
]}}`
const endpointsVendor = `{"data": {"endpoints": [
  {"tag": "together", "pricing": {"prompt": "0.0000001", "completion": "0.0000001"}},
  {"tag": "deepseek", "pricing": {"prompt": "0.00000027", "completion": "0.0000011", "input_cache_read": "0.00000007"}}
]}}`

func TestParseDatasets(t *testing.T) {
	l, err := parseLiteLLM([]byte(litellmFixture))
	if err != nil {
		t.Fatal(err)
	}
	clean := func(p Price) Price { c, _ := cleanPrice(p); return c }
	if p := clean(l["gpt-6.1-sol"]); p.Input != 2 || p.Output != 10 || p.Cached != 0.1 {
		t.Fatalf("official LiteLLM price: %+v", p)
	}
	if _, ok := l["gpt-6-luna"]; ok {
		t.Fatal("reseller (azure) price must not be used")
	}
	if _, ok := l["deepseek-v4.1-flash"]; ok {
		t.Fatal("non-official provider price must not be used")
	}
	if p := clean(l["gpt-image-2"]); p.Output != 30 || p.Input != 5 {
		t.Fatalf("image model output should use image token price: %+v", p)
	}
	if p := clean(l["claude-x"]); p.Cached != 0.3 {
		t.Fatalf("bare key should win over qualified: %+v", p)
	}
	o, err := parseOpenRouter([]byte(openrouterFixture))
	if err != nil {
		t.Fatal(err)
	}
	if o["deepseek-v4.1-flash"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("openrouter ids: %v", o)
	}
	if _, ok := o["gpt-6-luna"]; ok {
		t.Fatal("non-official OpenRouter vendor must not be used")
	}
	if _, ok := parseOpenRouterEndpoints([]byte(endpointsThirdParty), "deepseek"); ok {
		t.Fatal("third-party endpoint price must not count as the vendor's")
	}
	if p, ok := parseOpenRouterEndpoints([]byte(endpointsVendor), "deepseek"); !ok || clean(p).Input != 0.27 || clean(p).Cached != 0.07 {
		t.Fatalf("vendor endpoint price: %+v %v", p, ok)
	}
	for _, bad := range []string{"NaN", "-1", "", "1e400", "0.1abc"} {
		if _, ok := parseRate(bad); ok {
			t.Errorf("parseRate accepted %q", bad)
		}
	}
}

func TestSyncPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "litellm"):
			w.Write([]byte(litellmFixture))
		case strings.HasSuffix(r.URL.Path, "/deepseek/deepseek-v5/endpoints"):
			w.Write([]byte(endpointsVendor))
		case strings.HasSuffix(r.URL.Path, "/endpoints"):
			w.Write([]byte(endpointsThirdParty))
		default:
			w.Write([]byte(openrouterFixture))
		}
	}))
	defer srv.Close()

	a := newApp()
	db := filepath.Join(t.TempDir(), "s.db")
	cfg := "enabled: true\ndb_path: " + db + "\nprice_sync: false\nprice_litellm_url: " + srv.URL + "/litellm\nprice_openrouter_url: " + srv.URL + "/or\n"
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfg)})
	if _, err := a.dispatch(methodPluginRegister, raw); err != nil {
		t.Fatal(err)
	}
	defer a.shutdown()
	h := &harness{t: t, app: a}

	// Usage before any price: recorded at zero cost.
	_, u := h.mgmt("POST", "user/save", map[string]any{"name": "E", "extra_models": []string{"*"}})
	uid := int64(u["id"].(float64))
	_, k := h.mgmt("POST", "key/create", map[string]any{"user_id": uid})
	p := principalFor(uid, int64(k["id"].(float64)))
	at := time.Now()
	use := func(rid, alias, model string) {
		rec, _ := json.Marshal(usageRecord{RequestID: rid, APIKey: p, Model: model, Alias: alias, Generate: true, RequestedAt: at,
			Detail: usageDetail{InputTokens: 1_000_000, OutputTokens: 100_000, TotalTokens: 1_100_000}})
		h.call(methodUsageHandle, rec, nil)
	}
	for _, m := range []string{"gpt-6.1-sol", "deepseek-v4.1-flash", "codex-auto-review", "deepseek-v5"} {
		use(m, "", m)
	}
	// Same hour bucket "my-alias": one request priced via its upstream model
	// ($9+$0.9 manual deepseek-v4.1-flash price set below is not yet there,
	// so use a priced upstream), one unpriced. Backfill must add only the gap.
	h.mgmt("POST", "price/save", map[string]any{"model": "up-model", "input": 5})
	use("m1", "gpt-6.2-sol", "up-model") // priced $5 via upstream fallback
	use("m2", "gpt-6.2-sol", "none")     // unpriced
	// The owner's own prices must survive syncing, wildcards included.
	h.mgmt("POST", "price/save", map[string]any{"model": "deepseek-v4.1-flash", "input": 9, "output": 9})
	h.mgmt("POST", "price/save", map[string]any{"model": "claude-*", "input": 1, "output": 1})
	h.mgmt("POST", "group/save", map[string]any{"name": "c", "models": []string{"claude-x"}})

	st, err := a.syncPrices(context.Background())
	if err != nil {
		t.Fatalf("sync: %v %+v", err, st)
	}
	// Updated: gpt-6.1-sol (LiteLLM), deepseek-v5 (vendor endpoint), gpt-6.2-sol? not in
	// the fixture, so it stays unpriced. deepseek-v4.1-flash is manual.
	if st.Updated != 2 {
		t.Fatalf("sync status: %+v", st)
	}
	for _, m := range st.Unmatched {
		if m == "deepseek-v5" || m == "gpt-6.1-sol" {
			t.Fatalf("%s should be priced: %+v", m, st)
		}
	}
	_, state := h.mgmt("GET", "state", nil)
	prices := map[string]map[string]any{}
	for _, row := range state["prices"].([]any) {
		r := row.(map[string]any)
		prices[r["model"].(string)] = r
	}
	if prices["gpt-6.1-sol"]["source"] != "litellm" || prices["gpt-6.1-sol"]["input"].(float64) != 2 {
		t.Fatalf("auto price: %v", prices["gpt-6.1-sol"])
	}
	if prices["deepseek-v4.1-flash"]["source"] != "manual" || prices["deepseek-v4.1-flash"]["input"].(float64) != 9 {
		t.Fatalf("manual price overwritten: %v", prices["deepseek-v4.1-flash"])
	}
	if _, ok := prices["claude-x"]; ok {
		t.Fatalf("auto row written for a model covered by a manual wildcard: %v", prices["claude-x"])
	}
	// Manual wildcard beats an automatic exact row in lookups.
	tbl := newPriceTable([]Price{{Model: "gpt-6*", Input: 1, Source: "manual"}, {Model: "gpt-6.1-sol", Input: 2, Source: "litellm"}})
	if p, _ := tbl.lookup("gpt-6.1-sol"); p.Input != 1 {
		t.Fatalf("manual wildcard should win over auto exact: %+v", p)
	}
	if p, _ := newPriceTable([]Price{{Model: "gpt-6.1-sol", Input: 2, Source: "litellm"}}).lookup("gpt-6.1-sol"); p.Input != 2 {
		t.Fatalf("auto price lookup: %+v", p)
	}
	if prices["deepseek-v5"]["source"] != "openrouter" || prices["deepseek-v5"]["input"].(float64) != 0.27 {
		t.Fatalf("vendor endpoint price: %v", prices["deepseek-v5"])
	}
	// Backfill: gpt-6.1-sol 1M*2 + 100k*10 = $3; deepseek-v5 1M*0.27 + 100k*1.1 = $0.38;
	// plus the $5 up-model request recorded at request time, plus the manual
	// deepseek-v4.1-flash price backfilled when it was saved (1M*9 + 100k*9 = $9.9).
	q := state["users"].([]any)[0].(map[string]any)["quota"].(map[string]any)
	if got := q["total_cost"].(float64); got < 18.2799 || got > 18.2801 {
		t.Fatalf("backfilled total cost: %v", q)
	}
	if ps := state["price_sync"].(map[string]any); ps["last_at"].(float64) == 0 {
		t.Fatalf("price_sync status: %v", ps)
	}
	// A second sync with unchanged data changes nothing.
	if st, _ := a.syncPrices(context.Background()); st.Updated != 0 {
		t.Fatalf("idempotent sync updated %d", st.Updated)
	}
}

// A bucket mixing priced and unpriced requests grows by exactly the unpriced
// requests' new cost.
func TestBackfillMixedBucket(t *testing.T) {
	h := newHarness(t)
	_, u := h.mgmt("POST", "user/save", map[string]any{"name": "F", "extra_models": []string{"*"}})
	uid := int64(u["id"].(float64))
	_, k := h.mgmt("POST", "key/create", map[string]any{"user_id": uid})
	p := principalFor(uid, int64(k["id"].(float64)))
	h.mgmt("POST", "price/save", map[string]any{"model": "up-model", "input": 5})
	at := time.Now()
	for i, model := range []string{"up-model", "other"} {
		rec, _ := json.Marshal(usageRecord{RequestID: fmt.Sprint(i), APIKey: p, Alias: "alias-x", Model: model, Generate: true,
			RequestedAt: at, Detail: usageDetail{InputTokens: 1_000_000, TotalTokens: 1_000_000}})
		h.call(methodUsageHandle, rec, nil)
	}
	st := h.app.db()
	rows, _ := st.loadPrices()
	table := newPriceTable(append(rows, Price{Model: "alias-x", Input: 2, Source: "litellm"}))
	if err := backfillCosts(st.db, table, []string{"alias-x"}); err != nil {
		t.Fatal(err)
	}
	var hourly int64
	st.db.QueryRow(`SELECT SUM(cost_micros) FROM usage_hourly WHERE model = 'alias-x'`).Scan(&hourly)
	var logs int64
	st.db.QueryRow(`SELECT SUM(cost_micros) FROM request_log WHERE model = 'alias-x'`).Scan(&logs)
	if hourly != moneyToMicros(7) || logs != moneyToMicros(7) {
		t.Fatalf("mixed bucket: hourly=%d logs=%d, want 7000000", hourly, logs)
	}
}

func TestPricingReviewFixes(t *testing.T) {
	// Manual rules for any name beat automatic prices for any name.
	s := newState()
	s.setPrices([]Price{{Model: "alias-y", Input: 2, Source: "litellm"}, {Model: "up-*", Input: 7, Source: "manual"}})
	if p, _ := s.priceFor("alias-y", "up-model"); p.Input != 7 {
		t.Fatalf("manual upstream wildcard should beat auto alias price: %+v", p)
	}
	if p, _ := s.priceFor("alias-y", "other"); p.Input != 2 {
		t.Fatalf("auto alias price as fallback: %+v", p)
	}

	// A ledger in another currency never imports dollar prices.
	cfg, _ := parseConfig([]byte("currency_symbol: \"¥\"\n"))
	if cfg.priceSync() {
		t.Fatal("price sync must be off for a non-USD ledger")
	}
	cfg, _ = parseConfig(nil)
	if !cfg.priceSync() {
		t.Fatal("price sync should default on for $")
	}

	// A zero-cost bucket whose log rows were partly pruned is priced in full.
	h := newHarness(t)
	_, u := h.mgmt("POST", "user/save", map[string]any{"name": "G", "extra_models": []string{"*"}})
	uid := int64(u["id"].(float64))
	_, k := h.mgmt("POST", "key/create", map[string]any{"user_id": uid})
	p := principalFor(uid, int64(k["id"].(float64)))
	for i := 0; i < 2; i++ {
		rec, _ := json.Marshal(usageRecord{RequestID: fmt.Sprint("z", i), APIKey: p, Model: "zmodel", Generate: true,
			RequestedAt: time.Now(), Detail: usageDetail{InputTokens: 1_000_000, TotalTokens: 1_000_000}})
		h.call(methodUsageHandle, rec, nil)
	}
	st := h.app.db()
	st.db.Exec(`DELETE FROM request_log WHERE id = (SELECT MIN(id) FROM request_log WHERE model = 'zmodel')`)
	if err := backfillCosts(st.db, newPriceTable([]Price{{Model: "zmodel", Input: 2}}), []string{"zmodel"}); err != nil {
		t.Fatal(err)
	}
	var hourly int64
	st.db.QueryRow(`SELECT SUM(cost_micros) FROM usage_hourly WHERE model = 'zmodel'`).Scan(&hourly)
	if hourly != moneyToMicros(4) {
		t.Fatalf("pruned zero bucket: %d, want 4000000", hourly)
	}

	// "改回自动" refuses when the manual row changed after it was read.
	h.mgmt("POST", "price/save", map[string]any{"model": "zmodel", "input": 3})
	_, err := h.app.applyAutoPrices(context.Background(), st, []Price{{Model: "zmodel", Input: 2, Source: "litellm"}},
		&manualGuard{model: "zmodel", updatedAt: 1, price: Price{Input: 3}})
	if !errors.Is(err, errPriceChanged) {
		t.Fatalf("stale guard: %v", err)
	}
	// A different store (reconfigured meanwhile) is refused.
	other, _ := openStore(filepath.Join(t.TempDir(), "o.db"))
	defer other.close()
	if _, err := h.app.applyAutoPrices(context.Background(), other, []Price{{Model: "x", Input: 1, Source: "litellm"}}, nil); !errors.Is(err, errStoreChanged) {
		t.Fatalf("store switch: %v", err)
	}
}
