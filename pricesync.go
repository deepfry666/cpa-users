package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Price sync imports official list prices from public datasets so the owner
// does not type them in. Only exact model names from official channels are
// used; anything else stays unpriced for the owner to fill in. Manual rows are
// never touched.

const (
	defaultLiteLLMURL    = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	defaultOpenRouterURL = "https://openrouter.ai/api/v1/models"

	sourceManual     = "manual"
	sourceLiteLLM    = "litellm"
	sourceOpenRouter = "openrouter"

	maxDatasetBytes = 64 << 20
	// syncCooldown spaces out syncs triggered by newly seen models.
	syncCooldown = 10 * time.Minute
)

// litellmOfficial are LiteLLM providers that publish the vendor's own list
// price. Resellers and clouds (azure, bedrock, openrouter, ...) are skipped
// because their rates differ from the vendor's.
var litellmOfficial = map[string]bool{
	"openai": true, "text-completion-openai": true, "anthropic": true, "gemini": true,
	"vertex_ai-language-models": true, "deepseek": true, "xai": true, "mistral": true,
	"moonshot": true, "dashscope": true, "zai": true, "minimax": true, "volcengine": true,
}

// openrouterVendorTags maps an OpenRouter vendor prefix to the endpoint tags
// of the vendor's own API. A model's headline price on OpenRouter is often a
// third-party host's, so only the vendor's own endpoint counts.
var openrouterVendorTags = map[string][]string{
	"openai": {"openai"}, "anthropic": {"anthropic"}, "google": {"google-ai-studio"},
	"deepseek": {"deepseek"}, "x-ai": {"xai", "x-ai"}, "mistralai": {"mistral"},
	"moonshotai": {"moonshotai", "moonshot-ai"}, "qwen": {"alibaba"}, "z-ai": {"z-ai", "zai"},
	"minimax": {"minimax"},
}

type litellmEntry struct {
	Provider           string   `json:"litellm_provider"`
	Input              *float64 `json:"input_cost_per_token"`
	Output             *float64 `json:"output_cost_per_token"`
	CacheRead          *float64 `json:"cache_read_input_token_cost"`
	OutputImageToken   *float64 `json:"output_cost_per_image_token"`
	OutputPerImage     *float64 `json:"output_cost_per_image"`
	InputCostPerReq    *float64 `json:"input_cost_per_request"`
	OutputCostPerQuery *float64 `json:"output_cost_per_query"`
}

func perMillion(v *float64) float64 {
	if v == nil || *v < 0 {
		return 0
	}
	return *v * 1e6
}

// price converts an entry to per-million prices. Image generation models
// produce image tokens, so their output uses the image-token price.
func (e litellmEntry) price(name string) (Price, bool) {
	p := Price{Input: perMillion(e.Input), Cached: perMillion(e.CacheRead), Output: perMillion(e.Output)}
	if e.OutputImageToken != nil && (p.Output == 0 || strings.Contains(name, "image")) {
		p.Output = perMillion(e.OutputImageToken)
	}
	if e.CacheRead == nil {
		p.Cached = p.Input
	}
	if p.Input == 0 && p.Output == 0 && e.OutputPerImage != nil && *e.OutputPerImage > 0 {
		p.PerRequest = *e.OutputPerImage
	}
	ok := p.Input > 0 || p.Output > 0 || p.PerRequest > 0
	return p, ok
}

// parseLiteLLM keeps official-channel entries, keyed by bare model name.
// A bare key wins over a provider-qualified one.
func parseLiteLLM(raw []byte) (map[string]Price, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("LiteLLM 数据格式错误：%w", err)
	}
	out := map[string]Price{}
	qualified := map[string]Price{}
	for key, rawEntry := range all {
		var e litellmEntry
		if json.Unmarshal(rawEntry, &e) != nil || !litellmOfficial[e.Provider] {
			continue
		}
		name := strings.ToLower(key)
		p, ok := e.price(name)
		if !ok {
			continue
		}
		if prov, rest, found := strings.Cut(name, "/"); found {
			if !litellmOfficial[prov] || strings.Contains(rest, "/") {
				continue
			}
			qualified[rest] = p
			continue
		}
		out[name] = p
	}
	for name, p := range qualified {
		if _, ok := out[name]; !ok {
			out[name] = p
		}
	}
	return out, nil
}

// parseRate reads a per-token price string. OpenRouter uses "-1" or "" for
// variable pricing; those and anything non-finite are rejected.
func parseRate(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

// parseOpenRouter lists vendor-prefixed model IDs, keyed by bare model name.
// Prices come from each model's endpoints (see openRouterVendorPrice).
func parseOpenRouter(raw []byte) (map[string]string, error) {
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("OpenRouter 数据格式错误：%w", err)
	}
	out := map[string]string{}
	for _, m := range doc.Data {
		id := strings.ToLower(m.ID)
		vendor, name, ok := strings.Cut(id, "/")
		if !ok || openrouterVendorTags[vendor] == nil || strings.Contains(name, ":") || strings.Contains(name, "/") {
			continue // ":free", ":beta" and similar variants are not list prices
		}
		out[name] = id
	}
	return out, nil
}

// parseOpenRouterEndpoints returns the price of the vendor's own endpoint.
func parseOpenRouterEndpoints(raw []byte, vendor string) (Price, bool) {
	var doc struct {
		Data struct {
			Endpoints []struct {
				Tag     string `json:"tag"`
				Pricing struct {
					Prompt     string `json:"prompt"`
					Completion string `json:"completion"`
					CacheRead  string `json:"input_cache_read"`
					Request    string `json:"request"`
				} `json:"pricing"`
			} `json:"endpoints"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return Price{}, false
	}
	for _, e := range doc.Data.Endpoints {
		own := false
		for _, tag := range openrouterVendorTags[vendor] {
			if strings.EqualFold(e.Tag, tag) {
				own = true
			}
		}
		if !own {
			continue
		}
		in, ok1 := parseRate(e.Pricing.Prompt)
		out, ok2 := parseRate(e.Pricing.Completion)
		if !ok1 || !ok2 || (in == 0 && out == 0) {
			return Price{}, false
		}
		p := Price{Input: in * 1e6, Output: out * 1e6, Cached: in * 1e6}
		if cr, ok := parseRate(e.Pricing.CacheRead); ok {
			p.Cached = cr * 1e6
		}
		if req, ok := parseRate(e.Pricing.Request); ok {
			p.PerRequest = req
		}
		return p, true
	}
	return Price{}, false
}

// roundPrice trims float noise from per-token to per-million conversion.
func roundPrice(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// cleanPrice rounds a dataset price and rejects values the ledger cannot hold.
func cleanPrice(p Price) (Price, bool) {
	p.Input, p.Cached, p.Output, p.PerRequest = roundPrice(p.Input), roundPrice(p.Cached), roundPrice(p.Output), roundPrice(p.PerRequest)
	for _, v := range []float64{p.Input, p.Cached, p.Output, p.PerRequest} {
		if !validMoney(v) {
			return Price{}, false
		}
	}
	return p, p.Input > 0 || p.Output > 0 || p.PerRequest > 0
}

func samePrice(a, b Price) bool {
	return a.Input == b.Input && a.Cached == b.Cached && a.Output == b.Output && a.PerRequest == b.PerRequest
}

// ---- fetching ----

func (a *App) httpClient() *http.Client {
	cfg := a.config()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if cfg.PriceProxyURL != "" {
		if u, err := url.Parse(cfg.PriceProxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: 90 * time.Second, Transport: tr}
}

func fetchDataset(ctx context.Context, c *http.Client, src string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cpa-users-price-sync")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDatasetBytes))
}

// priceSources resolves model names to official prices: LiteLLM first, then
// the vendor's own OpenRouter endpoint.
type priceSources struct {
	litellm    map[string]Price
	openrouter map[string]string // bare name -> vendor/name
	orBase     string
	client     *http.Client
	sources    []string
	errs       []string
}

func (a *App) loadSources(ctx context.Context) (*priceSources, error) {
	cfg := a.config()
	ps := &priceSources{client: a.httpClient(), orBase: strings.TrimRight(cfg.PriceOpenRouterURL, "/")}
	if raw, err := fetchDataset(ctx, ps.client, cfg.PriceLiteLLMURL); err != nil {
		ps.errs = append(ps.errs, "LiteLLM："+err.Error())
	} else if ps.litellm, err = parseLiteLLM(raw); err != nil {
		ps.errs = append(ps.errs, err.Error())
	} else {
		ps.sources = append(ps.sources, fmt.Sprintf("LiteLLM（%d 个官方价）", len(ps.litellm)))
	}
	if raw, err := fetchDataset(ctx, ps.client, cfg.PriceOpenRouterURL); err != nil {
		ps.errs = append(ps.errs, "OpenRouter："+err.Error())
	} else if ps.openrouter, err = parseOpenRouter(raw); err != nil {
		ps.errs = append(ps.errs, err.Error())
	} else {
		ps.sources = append(ps.sources, "OpenRouter")
	}
	if len(ps.sources) == 0 {
		return nil, fmt.Errorf("价格数据源都获取失败：%s", strings.Join(ps.errs, "；"))
	}
	return ps, nil
}

func (ps *priceSources) resolve(ctx context.Context, model string) (Price, string, bool) {
	if p, ok := ps.litellm[model]; ok {
		if p, ok := cleanPrice(p); ok {
			return p, sourceLiteLLM, true
		}
	}
	id, ok := ps.openrouter[model]
	if !ok {
		return Price{}, "", false
	}
	raw, err := fetchDataset(ctx, ps.client, ps.orBase+"/"+id+"/endpoints")
	if err != nil {
		return Price{}, "", false
	}
	vendor, _, _ := strings.Cut(id, "/")
	if p, ok := parseOpenRouterEndpoints(raw, vendor); ok {
		if p, ok := cleanPrice(p); ok {
			return p, sourceOpenRouter, true
		}
	}
	return Price{}, "", false
}

// ---- sync ----

// SyncStatus is the last sync outcome shown in the console.
type SyncStatus struct {
	Enabled   bool     `json:"enabled"`
	Running   bool     `json:"running"`
	LastAt    int64    `json:"last_at"`
	NextAt    int64    `json:"next_at"`
	Error     string   `json:"error"`
	Matched   int      `json:"matched"`
	Updated   int      `json:"updated"`
	Unmatched []string `json:"unmatched"`
	Sources   []string `json:"sources"`
}

type priceSyncer struct {
	mu      sync.Mutex
	running bool
	status  SyncStatus
	last    time.Time
	trigger chan struct{}
}

func newPriceSyncer() *priceSyncer {
	return &priceSyncer{trigger: make(chan struct{}, 1)}
}

func (ps *priceSyncer) snapshot() SyncStatus {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	s := ps.status
	s.Running = ps.running
	s.Unmatched = append([]string{}, ps.status.Unmatched...)
	s.Sources = append([]string{}, ps.status.Sources...)
	return s
}

func (ps *priceSyncer) poke() {
	select {
	case ps.trigger <- struct{}{}:
	default:
	}
}

// syncTargets lists model names worth pricing: catalog models, models with
// usage, and exact (non-wildcard) names in groups and user lists.
func (a *App) syncTargets() []string {
	seen := map[string]bool{}
	add := func(m string) {
		m = normalizeModel(m)
		if m != "" && !strings.Contains(m, "*") {
			seen[m] = true
		}
	}
	for _, m := range a.state.knownModels() {
		add(m)
	}
	if st := a.db(); st != nil {
		if used, err := st.usedModels(time.Unix(0, 0)); err == nil {
			for _, m := range used {
				add(m)
			}
		}
	}
	groups, users, _ := a.state.snapshot()
	for _, g := range groups {
		for _, m := range g.Models {
			add(m)
		}
	}
	for _, u := range users {
		for _, m := range u.ExtraModels {
			add(m)
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// syncPrices fetches the datasets and updates automatic price rows. Newly
// priced models get their unpriced history filled in; history that already
// carries a cost keeps the price recorded at request time.
func (a *App) syncPrices(ctx context.Context) (SyncStatus, error) {
	started := a.db()
	ps := a.syncer
	ps.mu.Lock()
	if ps.running {
		ps.mu.Unlock()
		return ps.snapshot(), errSyncRunning
	}
	ps.running = true
	ps.mu.Unlock()

	status, err := a.doSyncPrices(ctx)

	ps.mu.Lock()
	ps.running = false
	ps.last = time.Now()
	status.Enabled = a.config().priceSync()
	status.LastAt = ps.last.Unix()
	if err != nil {
		status.Error = err.Error()
	}
	if status.Enabled {
		status.NextAt = ps.last.Add(a.config().syncInterval()).Unix()
	}
	ps.status = status
	ps.mu.Unlock()
	if st := a.db(); st != nil && st == started && ctx.Err() == nil {
		raw, _ := json.Marshal(status)
		_ = st.setMeta("price_sync", string(raw))
	}
	return status, err
}

var (
	errNotUSD       = fmt.Errorf("金额货币不是美元（currency_symbol），官方价格是美元，自动同步已停用")
	errStoreChanged = fmt.Errorf("同步期间插件配置或数据库已切换，本次结果已丢弃")
	errSyncRunning  = fmt.Errorf("同步正在进行")
	errPriceChanged = fmt.Errorf("这个模型的手动价格刚被修改过，请刷新后再试")
)

func (a *App) doSyncPrices(ctx context.Context) (SyncStatus, error) {
	var status SyncStatus
	st := a.db()
	if st == nil {
		return status, fmt.Errorf("插件存储不可用")
	}
	if !a.config().usdLedger() {
		return status, errNotUSD
	}
	src, err := a.loadSources(ctx)
	if err != nil {
		return status, err
	}
	status.Sources = src.sources
	if len(src.errs) > 0 {
		status.Error = "部分数据源失败：" + strings.Join(src.errs, "；")
	}

	before := a.state.priceTable()
	var updates []Price
	for _, m := range a.syncTargets() {
		p, source, ok := src.resolve(ctx, m)
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		if !ok {
			if _, priced := before.lookup(m); !priced {
				status.Unmatched = append(status.Unmatched, m)
			}
			continue
		}
		status.Matched++
		if _, manual := before.manualLookup(m); manual {
			continue // the owner's own price (exact or wildcard) always wins
		}
		if cur, ok := before.auto.exact[m]; ok && cur.Source == source && samePrice(cur, p) {
			continue
		}
		p.Model, p.Source = m, source
		updates = append(updates, p)
	}
	if len(updates) == 0 {
		return status, nil
	}
	n, err := a.applyAutoPrices(ctx, st, updates, nil)
	status.Updated = n
	return status, err
}

// manualGuard identifies the exact manual row "改回自动" may replace.
type manualGuard struct {
	model     string
	updatedAt int64
	price     Price
}

// applyAutoPrices writes synced prices and fills in history for models that
// had no price, in one transaction, then publishes the new prices and quota
// counters. replaceManual names a manual row to swap for its automatic price.
func (a *App) applyAutoPrices(ctx context.Context, st *store, updates []Price, replaceManual *manualGuard) (int, error) {
	// Same order as management writes that recompute: writeMu, then acctMu.
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.acctMu.Lock()
	defer a.acctMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// The download ran without locks; publish only into the storage it
	// started with and only while the ledger is still in dollars.
	if a.db() != st {
		return 0, errStoreChanged
	}
	if !a.config().usdLedger() {
		return 0, errNotUSD
	}

	before := a.state.priceTable()
	tx, err := st.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if replaceManual != nil {
		g := replaceManual
		res, err := tx.Exec(`DELETE FROM prices WHERE model = ? AND source = 'manual' AND updated_at = ?
			AND input = ? AND cached = ? AND output = ? AND per_request = ? AND note = ?`,
			g.model, g.updatedAt, g.price.Input, g.price.Cached, g.price.Output, g.price.PerRequest, g.price.Note)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, errPriceChanged
		}
	}
	written := 0
	var newlyPriced []string
	for _, p := range updates {
		res, err := tx.Exec(`INSERT INTO prices (model, input, cached, output, per_request, note, source, updated_at)
			VALUES (?, ?, ?, ?, ?, '', ?, ?)
			ON CONFLICT (model) DO UPDATE SET input = excluded.input, cached = excluded.cached, output = excluded.output,
				per_request = excluded.per_request, source = excluded.source, updated_at = excluded.updated_at
			WHERE prices.source != 'manual'`,
			p.Model, p.Input, p.Cached, p.Output, p.PerRequest, p.Source, time.Now().Unix())
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // the owner saved a manual price meanwhile
		}
		written++
		if _, ok := before.lookup(p.Model); !ok {
			newlyPriced = append(newlyPriced, p.Model)
		}
	}
	if written == 0 && replaceManual == nil {
		return 0, nil
	}
	rows, err := loadPricesFrom(tx)
	if err != nil {
		return 0, err
	}
	table := newPriceTable(rows)
	if len(newlyPriced) > 0 {
		if err := backfillCosts(tx, table, newlyPriced); err != nil {
			return 0, fmt.Errorf("补算历史金额失败：%w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := a.reload(); err != nil {
		return written, err
	}
	a.seedCountersLocked()
	return written, nil
}

// revertToAuto replaces a manual price with the official one. Nothing changes
// when no official price is found.
func (a *App) revertToAuto(ctx context.Context, model string) (Price, error) {
	st := a.db()
	if st == nil {
		return Price{}, fmt.Errorf("插件存储不可用")
	}
	if !a.config().usdLedger() {
		return Price{}, errNotUSD
	}
	guard := manualGuard{model: model, updatedAt: -1}
	if rows, err := st.loadPrices(); err == nil {
		for _, p := range rows {
			if p.Model == model && p.Source == sourceManual {
				guard.updatedAt, guard.price = p.SyncedAt, p // SyncedAt holds updated_at
			}
		}
	}
	if guard.updatedAt < 0 {
		return Price{}, fmt.Errorf("没有找到这个模型的手动价格")
	}
	src, err := a.loadSources(ctx)
	if err != nil {
		return Price{}, err
	}
	m := normalizeModel(model)
	p, source, ok := src.resolve(ctx, m)
	if !ok {
		return Price{}, errNoOfficialPrice
	}
	p.Model, p.Source = m, source
	if _, err := a.applyAutoPrices(ctx, st, []Price{p}, &guard); err != nil {
		return Price{}, err
	}
	return p, nil
}

var errNoOfficialPrice = fmt.Errorf("官方价格表里查不到这个模型，手动价格保持不变")

// priceSyncLoop runs the periodic sync and syncs soon after new models show
// up, at most once per cooldown. It exits when ctx is cancelled.
func (a *App) priceSyncLoop(ctx context.Context) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	next := time.Now().Add(30 * time.Second)
	startupDone := false
	schedule := func(d time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
		next = time.Now().Add(d)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.syncer.trigger:
			a.syncer.mu.Lock()
			last := a.syncer.last
			a.syncer.mu.Unlock()
			if !startupDone {
				continue // the startup run covers it
			}
			// Run when the cooldown ends, unless something is due sooner.
			if due := time.Until(last.Add(syncCooldown)); due > 0 {
				if time.Now().Add(due).Before(next) {
					schedule(due)
				}
				continue
			}
		case <-timer.C:
		}
		startupDone = true
		interval := a.config().syncInterval()
		if a.config().priceSync() && a.db() != nil {
			runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			_, err := a.syncPrices(runCtx)
			cancel()
			if errors.Is(err, errSyncRunning) {
				interval = time.Minute // a console sync is running; look again after it
			} else if err != nil && ctx.Err() == nil {
				log.Printf("[%s] price sync: %v", PluginID, err)
			}
		}
		schedule(interval)
	}
}
