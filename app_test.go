package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CPA's validPlugin drops a plugin whose metadata lacks any of these fields.
func TestRegistrationMetadataAccepted(t *testing.T) {
	reg := newApp().registration()
	m := reg.Metadata
	if m.Name == "" || m.Version == "" || m.Author == "" || m.GitHubRepository == "" {
		t.Fatalf("metadata incomplete, CPA will refuse the plugin: %+v", m)
	}
	if strings.HasPrefix(m.Version, "v") {
		t.Fatalf("CPA rejects versions starting with v: %q", m.Version)
	}
	raw, _ := json.Marshal(reg)
	for _, field := range []string{`"Name"`, `"GitHubRepository"`, `"frontend_auth_provider":true`, `"usage_plugin":true`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("registration JSON missing %s: %s", field, raw)
		}
	}
}

func TestGlobAndNormalize(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"*", "anything", true},
		{"gpt-5*", "gpt-5.6-sol", true},
		{"gpt-5*", "gpt-4o", false},
		{"claude-*-sonnet*", "claude-4-sonnet-latest", true},
		{"anthropic/*", "anthropic/claude", true},
		{"gemini-2.5-pro", "models/gemini-2.5-pro", true},
		{"gemini-2.5-pro", "gemini-2.5-pro(8192)", true},
		{"deepseek-v4-pro", "DeepSeek-V4-Pro", true},
		{"deepseek-v4-pro", "deepseek-v4-pro-max", false},
	}
	for _, c := range cases {
		got := modelAllowed(normalizePatterns([]string{c.pattern}), c.model)
		if got != c.want {
			t.Errorf("modelAllowed(%q, %q) = %v, want %v", c.pattern, c.model, got, c.want)
		}
	}
	if modelAllowed(nil, "gpt-5") {
		t.Error("empty allowlist must deny")
	}
}

func TestPrincipalRoundTrip(t *testing.T) {
	p := principalFor(12, 345)
	u, k, ok := parsePrincipal(p)
	if !ok || u != 12 || k != 345 {
		t.Fatalf("parsePrincipal(%q) = %d %d %v", p, u, k, ok)
	}
	if _, _, ok := parsePrincipal("sk-native-key"); ok {
		t.Fatal("native keys must not parse as plugin principals")
	}
	// Matches CLIProxyAPI sdk/cliproxy/session.CallerScope for the same input.
	if got := callerScope("abc"); len(got) != 64 {
		t.Fatalf("caller scope length %d", len(got))
	}
}

func TestCatalogFilterFormats(t *testing.T) {
	allow := func(id string) bool { return id == "a" || id == "c" }
	openai := []byte(`{"object":"list","data":[{"id":"a","object":"model"},{"id":"b"},{"id":"c"}]}`)
	c, ok := parseModelCatalog(openai)
	if !ok {
		t.Fatal("openai catalog not parsed")
	}
	out, n := c.filter(allow)
	if n != 2 || strings.Contains(string(out), `"b"`) || !strings.Contains(string(out), `"object":"list"`) {
		t.Fatalf("openai filter: %s", out)
	}

	claude := []byte(`{"data":[{"id":"a","type":"model"},{"id":"b"},{"id":"c"}],"first_id":"a","last_id":"c","has_more":false}`)
	c, _ = parseModelCatalog(claude)
	out, _ = c.filter(func(id string) bool { return id == "b" })
	var cl map[string]any
	_ = json.Unmarshal(out, &cl)
	if cl["first_id"] != "b" || cl["last_id"] != "b" {
		t.Fatalf("claude pagination not updated: %s", out)
	}

	// CPA cloaks non-Claude IDs for Claude Code; matching must use the real name.
	cloaked := []byte(`{"data":[{"id":"claude-opus-x"},{"id":"claude-fable-5-dd-hsalf-4v-keespeed"}],"first_id":"claude-opus-x","last_id":"x","has_more":false}`)
	c, _ = parseModelCatalog(cloaked)
	if c.ids[1] != "deepseek-v4-flash" {
		t.Fatalf("cloaked id not resolved: %v", c.ids)
	}
	out, n = c.filter(func(id string) bool { return id == "deepseek-v4-flash" })
	if n != 1 || !strings.Contains(string(out), `"first_id":"claude-fable-5-dd-hsalf-4v-keespeed"`) {
		t.Fatalf("cloaked entry and pagination must keep the listed id: %s", out)
	}

	gemini := []byte(`{"models":[{"name":"models/a"},{"name":"models/b"}]}`)
	c, ok = parseModelCatalog(gemini)
	if !ok || c.ids[0] != "a" {
		t.Fatalf("gemini ids: %v", c.ids)
	}
	if _, ok := parseModelCatalog([]byte(`{"id":"chatcmpl-1","choices":[]}`)); ok {
		t.Fatal("chat completion must not parse as catalog")
	}
}

func TestConcurrencyIsIdempotent(t *testing.T) {
	s := newState()
	now := time.Now()
	ok := func(id string) bool { r, _ := s.acquire(id, 1, 1, 0, now); return r == admitted }
	if !ok("r1") || !ok("r1") {
		t.Fatal("same request must be admitted twice")
	}
	if ok("r2") {
		t.Fatal("second request must be refused at max 1")
	}
	s.release("r1")
	s.release("r1")
	if !ok("r2") {
		t.Fatal("slot not released")
	}
	if s.running[1] != 1 {
		t.Fatalf("running = %d", s.running[1])
	}
	// Another user's request ID must not ride on an existing slot.
	if r, _ := s.acquire("r2", 2, 1, 0, now); r != admitted || s.running[2] != 1 {
		t.Fatalf("user 2 reusing r2: %v running=%d", r, s.running[2])
	}
}

func TestRPMWindow(t *testing.T) {
	s := newState()
	now := time.Now()
	for i := 0; i < 3; i++ {
		if r, _ := s.acquire(fmt.Sprintf("r%d", i), 1, 0, 3, now.Add(time.Duration(i)*time.Second)); r != admitted {
			t.Fatalf("request %d refused", i)
		}
	}
	r, retry := s.acquire("r3", 1, 0, 3, now.Add(10*time.Second))
	if r != deniedRPM || !retry.Equal(now.Add(time.Minute)) {
		t.Fatalf("want RPM denial retrying at +60s, got %v %v", r, retry.Sub(now))
	}
	if r, _ := s.acquire("r4", 1, 0, 3, now.Add(61*time.Second)); r != admitted {
		t.Fatal("window did not slide")
	}
	// At +61s only r2 (+2s) and r4 remain in the window.
	if q := s.quota(1, now.Add(61*time.Second)); q.RPM != 2 {
		t.Fatalf("rpm = %d", q.RPM)
	}
}

func TestCostAndPriceLookup(t *testing.T) {
	table := newPriceTable([]Price{
		{Model: "gpt-6*", Input: 1, Cached: 0.1, Output: 8},
		{Model: "gpt-6.1-sol", Input: 2, Cached: 0.2, Output: 16},
		{Model: "gpt-image-*", PerRequest: 0.04},
	})
	p, ok := table.lookup("GPT-6.1-sol(high)")
	if !ok || p.Input != 2 {
		t.Fatalf("exact price should win: %+v %v", p, ok)
	}
	if p, ok := table.lookup("gpt-6-luna"); !ok || p.Input != 1 {
		t.Fatalf("pattern price: %+v %v", p, ok)
	}
	if _, ok := table.lookup("deepseek-v4"); ok {
		t.Fatal("unpriced model matched")
	}
	// 1M input of which 600k cached, 100k output (reasoning included).
	got := costMicros(Price{Input: 2, Cached: 0.2, Output: 16}, tokenUsage{Input: 1_000_000, Cached: 600_000, Output: 100_000, Reasoning: 50_000, Total: 1_100_000, Successes: 1})
	if want := moneyToMicros(0.8 + 0.12 + 1.6); got != want {
		t.Fatalf("cost = %d, want %d", got, want)
	}
	price := Price{Input: 3, Cached: 0.3, Output: 15}
	for _, c := range []struct {
		name string
		d    usageDetail
		want float64
	}{
		// OpenAI: cached is part of input, reasoning part of output.
		{"openai", usageDetail{InputTokens: 1000, CachedTokens: 800, OutputTokens: 100, ReasoningTokens: 60, TotalTokens: 1100}, (200*3 + 800*0.3 + 100*15) / 1e6},
		// Claude: input excludes cache reads/writes; output includes thinking.
		{"claude", usageDetail{InputTokens: 50, CachedTokens: 900, CacheReadTokens: 900, CacheCreationTokens: 200, OutputTokens: 100,
			ReasoningTokens: 40, TotalTokens: 1250}, ((50+200)*3 + 900*0.3 + 100*15) / 1e6},
		// Claude cache write only: CPA copies writes into CachedTokens, but they are not reads.
		{"claude-write", usageDetail{InputTokens: 10, CachedTokens: 500, CacheCreationTokens: 500, OutputTokens: 20, TotalTokens: 530},
			(510*3 + 20*15) / 1e6},
		// Gemini: thoughts are reported apart from output.
		{"gemini", usageDetail{InputTokens: 1000, CachedTokens: 400, OutputTokens: 100, ReasoningTokens: 300, TotalTokens: 1400},
			(600*3 + 400*0.3 + 400*15) / 1e6},
	} {
		if got, want := costMicros(price, normalizeUsage(c.d)), moneyToMicros(c.want); got != want {
			t.Errorf("%s: cost %d micros, want %d (%+v)", c.name, got, want, normalizeUsage(c.d))
		}
	}
	if got := costMicros(Price{PerRequest: 0.04}, tokenUsage{Successes: 1}); got != 40_000 {
		t.Fatalf("per request = %d", got)
	}
}

func TestCostLimits(t *testing.T) {
	s := newState()
	now := time.Now()
	s.seedCounters(time.Local, now, nil, map[int64]int64{5: moneyToMicros(9.5)})
	s.addUsage(5, now, 100, moneyToMicros(0.6))
	q := s.quota(5, now)
	if q.Day.Cost != 0.6 || q.TotalCost != 10.1 {
		t.Fatalf("quota costs: %+v", q)
	}
	if r, _ := quotaExceeded(User{DailyCostLimit: 1}, q); r != "" {
		t.Fatalf("daily limit not reached yet: %s", r)
	}
	if r, at := quotaExceeded(User{TotalCostLimit: 10}, q); r == "" || !at.IsZero() {
		t.Fatalf("total limit should block forever: %q %v", r, at)
	}
	if r, _ := quotaExceeded(User{DailyCostLimit: 0.5}, q); r == "" {
		t.Fatal("daily cost limit should block")
	}
}

func TestQuotaRollsOverAtMidnight(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	s := newState()
	// addUsage rolls windows with the real clock, so the test day is today.
	day1 := startOfDay(time.Now(), loc).Add(time.Hour)
	s.seedCounters(loc, day1, nil, nil)
	s.addUsage(7, day1, 500, 0)
	if q := s.quota(7, day1); q.Day.Tokens != 500 || q.Month.Tokens != 500 {
		t.Fatalf("quota after usage: %+v", q)
	}
	day2 := day1.Add(24 * time.Hour)
	q := s.quota(7, day2)
	sameMonth := day2.In(loc).Month() == day1.In(loc).Month()
	if q.Day.Tokens != 0 || (sameMonth && q.Month.Tokens != 500) {
		t.Fatalf("day did not roll over: %+v", q)
	}
	u := User{DailyTokens: 400}
	if reason, _ := quotaExceeded(u, s.quota(7, day1)); reason != "" {
		// day1 is in the past relative to the rolled window; nothing used today.
		t.Fatalf("unexpected block: %s", reason)
	}
}

// ---- end-to-end through the RPC dispatcher ----

type harness struct {
	t   *testing.T
	app *App
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	a := newApp()
	db := filepath.Join(t.TempDir(), "t.db")
	cfg := "enabled: true\ndb_path: " + db + "\ntimezone: Asia/Shanghai\nprice_sync: false\n"
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfg)})
	h := &harness{t: t, app: a}
	var reg registration
	h.call(methodPluginRegister, raw, &reg)
	if !reg.Capabilities.FrontendAuthProvider || !reg.Capabilities.ResponseInterceptor {
		t.Fatalf("capabilities: %+v", reg.Capabilities)
	}
	t.Cleanup(a.shutdown)
	return h
}

func (h *harness) call(method string, payload []byte, out any) {
	h.t.Helper()
	raw, err := h.app.dispatch(method, payload)
	if err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		h.t.Fatalf("%s envelope: %s", method, raw)
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			h.t.Fatalf("%s result: %v", method, err)
		}
	}
}

func (h *harness) mgmt(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	raw, _ := json.Marshal(managementRequest{Method: method, Path: mgmtBase + path, Body: b, Query: url.Values{}})
	var resp managementResponse
	h.call(methodManagementHandle, raw, &resp)
	var out map[string]any
	_ = json.Unmarshal(resp.Body, &out)
	return resp.StatusCode, out
}

func (h *harness) auth(key string) frontendAuthResponse {
	raw, _ := json.Marshal(frontendAuthRequest{Method: "POST", Path: "/v1/chat/completions",
		Headers: http.Header{"Authorization": []string{"Bearer " + key}}})
	var resp frontendAuthResponse
	h.call(methodFrontendAuthAuthenticate, raw, &resp)
	return resp
}

func (h *harness) intercept(principal, requestID, model string) requestInterceptResponse {
	raw, _ := json.Marshal(requestInterceptRequest{RequestID: requestID, SourceFormat: "openai",
		RequestedModel: model, CallerScope: callerScope(principal), Metadata: map[string]any{"generate": true}})
	var resp requestInterceptResponse
	h.call(methodRequestInterceptBefore, raw, &resp)
	return resp
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)

	status, g := h.mgmt("POST", "group/save", map[string]any{"name": "基础", "models": []string{"gpt-5*", "deepseek-v4-flash"}})
	if status != 200 {
		t.Fatalf("group save: %d %v", status, g)
	}
	groupID := int64(g["id"].(float64))
	status, u := h.mgmt("POST", "user/save", map[string]any{"name": "A", "group_id": groupID, "extra_models": []string{"kimi-k3"},
		"daily_requests": 2, "max_concurrency": 1})
	if status != 200 {
		t.Fatalf("user save: %d %v", status, u)
	}
	userID := int64(u["id"].(float64))
	if status, dup := h.mgmt("POST", "user/save", map[string]any{"name": "A"}); status != 409 {
		t.Fatalf("duplicate name: %d %v", status, dup)
	}
	_, k := h.mgmt("POST", "key/create", map[string]any{"user_id": userID, "name": "laptop"})
	key := k["key"].(string)
	keyID := int64(k["id"].(float64))
	if !strings.HasPrefix(key, "sk-cu-") || len(key) != len("sk-cu-")+40 {
		t.Fatalf("key format: %q", key)
	}

	// Auth: our key authenticates, native and unknown keys fall through.
	a := h.auth(key)
	if !a.Authenticated || a.Principal != principalFor(userID, keyID) {
		t.Fatalf("auth: %+v", a)
	}
	if r := h.auth("sk-native-123"); r.Authenticated || r.DenyStatusCode != 0 {
		t.Fatalf("native key must not be handled: %+v", r)
	}
	if r := h.auth("sk-cu-unknown"); r.Authenticated || r.DenyStatusCode != 0 {
		t.Fatalf("unknown key must fall through: %+v", r)
	}

	// Model gate.
	p := a.Principal
	if r := h.intercept(p, "r1", "gpt-5.6-sol"); r.Terminate {
		t.Fatalf("allowed model refused: %s", r.ResponseBody)
	}
	if r := h.intercept(p, "r2", "claude-opus"); !r.Terminate || r.StatusCode != 403 {
		t.Fatalf("disallowed model not refused: %+v", r)
	}
	// Concurrency 1: r1 is still in flight.
	if r := h.intercept(p, "r3", "kimi-k3"); !r.Terminate || r.StatusCode != 429 {
		t.Fatalf("concurrency not enforced: %+v", r)
	}
	done, _ := json.Marshal(requestCompletion{RequestID: "r1", Outcome: "succeeded"})
	h.call(methodRequestComplete, done, nil)

	// Native keys pass through untouched.
	if r := h.intercept("sk-native-123", "n1", "claude-opus"); r.Terminate {
		t.Fatal("native key traffic must not be gated")
	}

	// Usage accounting: two successful requests exhaust daily_requests=2.
	for i, rid := range []string{"r1", "r4"} {
		rec, _ := json.Marshal(usageRecord{RequestID: rid, APIKey: p, Model: "gpt-5.6-sol", Provider: "codex", Generate: true,
			RequestedAt: time.Now(), Latency: 1500 * time.Millisecond,
			Detail: usageDetail{InputTokens: 100, OutputTokens: 50 + int64(i)}})
		h.call(methodUsageHandle, rec, nil)
	}
	if r := h.intercept(p, "r5", "gpt-5.6-sol"); !r.Terminate || r.StatusCode != 429 {
		t.Fatalf("daily request quota not enforced: %+v", r)
	}
	if !strings.Contains(string(h.intercept(p, "r6", "gpt-5.6-sol").ResponseBody), "今日请求次数已用完") {
		t.Fatal("quota message missing")
	}

	// Usage report and state snapshot.
	_, rep := h.mgmt("GET", "usage", nil)
	totals := rep["totals"].(map[string]any)
	if totals["requests"].(float64) != 2 || totals["total_tokens"].(float64) != 301 {
		t.Fatalf("usage totals: %v", totals)
	}
	_, st := h.mgmt("GET", "state", nil)
	users := st["users"].([]any)
	quota := users[0].(map[string]any)["quota"].(map[string]any)["day"].(map[string]any)
	if quota["requests"].(float64) != 2 {
		t.Fatalf("state quota: %v", quota)
	}

	// Model list filtering uses the raw Authorization header.
	catalog := []byte(`{"object":"list","data":[{"id":"gpt-5.6-sol"},{"id":"claude-opus"},{"id":"kimi-k3"},{"id":"deepseek-v4-pro"}]}`)
	raw, _ := json.Marshal(responseInterceptRequest{SourceFormat: "openai",
		RequestHeaders: http.Header{"Authorization": []string{"Bearer " + key}}, Body: catalog, StatusCode: 200})
	var filtered responseInterceptResponse
	h.call(methodResponseInterceptAfter, raw, &filtered)
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.Unmarshal(filtered.Body, &list)
	if len(list.Data) != 2 || list.Data[0].ID != "gpt-5.6-sol" || list.Data[1].ID != "kimi-k3" {
		t.Fatalf("filtered catalog: %s", filtered.Body)
	}
	// Catalog entries are remembered for the console.
	if got := h.app.state.knownModels(); len(got) < 4 {
		t.Fatalf("known models: %v", got)
	}

	// Self-service data authenticates with the user's key.
	meReq, _ := json.Marshal(managementRequest{Method: "GET", Path: resourceBase + "me/data",
		Headers: http.Header{"Authorization": []string{"Bearer " + key}}})
	var me managementResponse
	h.call(methodManagementHandle, meReq, &me)
	if me.StatusCode != 200 || !strings.Contains(string(me.Body), `"name":"A"`) {
		t.Fatalf("me: %d %s", me.StatusCode, me.Body)
	}

	// Disabling the key denies auth with a message.
	h.mgmt("POST", "key/update", map[string]any{"id": keyID, "name": "laptop", "disabled": true})
	if r := h.auth(key); r.Authenticated || r.DenyStatusCode != 401 {
		t.Fatalf("disabled key: %+v", r)
	}
	// Disabling the user wins over a re-enabled key.
	h.mgmt("POST", "key/update", map[string]any{"id": keyID, "name": "laptop", "disabled": false})
	h.mgmt("POST", "user/save", map[string]any{"id": userID, "name": "A", "group_id": groupID, "disabled": true})
	if r := h.auth(key); r.Authenticated || r.DenyStatusCode != 403 {
		t.Fatalf("disabled user: %+v", r)
	}

	// Counters survive a restart because they are seeded from SQLite.
	cfgRaw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte("enabled: true\ndb_path: " + h.app.config().DBPath + "\ntimezone: Asia/Shanghai\n")})
	h.app.shutdown()
	h.call(methodPluginRegister, cfgRaw, nil)
	if q := h.app.state.quota(userID, time.Now()); q.Day.Requests != 2 {
		t.Fatalf("counters after restart: %+v", q)
	}
}

func (h *harness) authPath(key, method, path string) frontendAuthResponse {
	raw, _ := json.Marshal(frontendAuthRequest{Method: method, Path: path,
		Headers: http.Header{"Authorization": []string{"Bearer " + key}}})
	var resp frontendAuthResponse
	h.call(methodFrontendAuthAuthenticate, raw, &resp)
	return resp
}

func (h *harness) interceptMeta(principal, requestID, requested, model string, meta map[string]any) requestInterceptResponse {
	raw, _ := json.Marshal(requestInterceptRequest{RequestID: requestID, SourceFormat: "openai",
		RequestedModel: requested, Model: model, CallerScope: callerScope(principal), Metadata: meta})
	var resp requestInterceptResponse
	h.call(methodRequestInterceptBefore, raw, &resp)
	return resp
}

func TestSharingHardening(t *testing.T) {
	h := newHarness(t)
	_, g := h.mgmt("POST", "group/save", map[string]any{"name": "文本", "models": []string{"gpt-6*"}})
	gid := g["id"].(float64)
	_, u := h.mgmt("POST", "user/save", map[string]any{"name": "B", "group_id": gid, "daily_requests": 1})
	uid := int64(u["id"].(float64))
	_, k := h.mgmt("POST", "key/create", map[string]any{"user_id": uid})
	key, kid := k["key"].(string), int64(k["id"].(float64))
	p := principalFor(uid, kid)

	// Endpoints that bypass admission are closed; images need an image model.
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/v1/alpha/search", 403}, {"GET", "/v1/realtime", 403}, {"POST", "/v1/live", 403},
		{"POST", "/v1/videos", 403}, {"POST", "/v1/images/generations", 403},
		{"POST", "/v1/responses", 0}, {"GET", "/v1/responses", 0}, {"GET", "/v1/models", 0},
		{"POST", "/v1beta/models/gpt-6:generateContent", 0},
	} {
		r := h.authPath(key, c.method, c.path)
		if r.DenyStatusCode != c.want || (c.want == 0) != r.Authenticated {
			t.Errorf("%s %s: %+v", c.method, c.path, r)
		}
	}
	// Query-string keys on the catalog would skip filtering.
	raw, _ := json.Marshal(frontendAuthRequest{Method: "GET", Path: "/v1/models", Query: url.Values{"key": []string{key}}})
	var qa frontendAuthResponse
	h.call(methodFrontendAuthAuthenticate, raw, &qa)
	if qa.Authenticated || qa.DenyStatusCode != 400 {
		t.Fatalf("query key on catalog: %+v", qa)
	}

	// Alias resolving outside the list, and missing model names, are refused.
	if r := h.interceptMeta(p, "a1", "gpt-6-luna", "claude-opus", nil); !r.Terminate || r.StatusCode != 403 {
		t.Fatalf("alias target not gated: %+v", r)
	}
	if r := h.interceptMeta(p, "a2", "", "", nil); !r.Terminate || r.StatusCode != 400 {
		t.Fatalf("empty model: %+v", r)
	}
	// generate=false from the client does not skip quotas.
	rec, _ := json.Marshal(usageRecord{RequestID: "a3", APIKey: p, Model: "gpt-6-luna", Generate: true, Failed: true,
		RequestedAt: time.Now(), Failure: usageFailure{StatusCode: 500}})
	h.call(methodUsageHandle, rec, nil) // failed attempts count toward request limits
	if r := h.interceptMeta(p, "a4", "gpt-6-luna", "", map[string]any{"generate": false}); !r.Terminate || r.StatusCode != 429 {
		t.Fatalf("generate=false bypassed quota: %+v", r)
	}

	// Deleting a key revokes scopes that already authenticated.
	h.mgmt("POST", "key/delete", map[string]any{"id": kid})
	if r := h.interceptMeta(p, "a5", "gpt-6-luna", "", nil); !r.Terminate || r.StatusCode != 401 {
		t.Fatalf("revoked key passed: %+v", r)
	}

	// IDs are not reused after deletion, so history stays with its owner.
	h.mgmt("POST", "user/delete", map[string]any{"id": uid})
	_, u2 := h.mgmt("POST", "user/save", map[string]any{"name": "C"})
	if int64(u2["id"].(float64)) <= uid {
		t.Fatalf("user id reused: %v after %d", u2["id"], uid)
	}
	_, k2 := h.mgmt("POST", "key/create", map[string]any{"user_id": u2["id"]})
	if int64(k2["id"].(float64)) <= kid {
		t.Fatalf("key id reused: %v after %d", k2["id"], kid)
	}
}

func TestCostEndToEnd(t *testing.T) {
	h := newHarness(t)
	if s, r := h.mgmt("POST", "price/save", map[string]any{"model": "gpt-6*", "input": 2, "cached": 0.2, "output": 16}); s != 200 {
		t.Fatalf("price save: %d %v", s, r)
	}
	if s, _ := h.mgmt("POST", "price/save", map[string]any{"model": "x", "input": -1}); s != 400 {
		t.Fatalf("negative price accepted: %d", s)
	}
	_, u := h.mgmt("POST", "user/save", map[string]any{"name": "D", "extra_models": []string{"gpt-6*", "deepseek*"},
		"daily_cost_limit": 2.5, "max_concurrency": 3, "rpm_limit": 30})
	uid := int64(u["id"].(float64))
	_, k := h.mgmt("POST", "key/create", map[string]any{"user_id": uid})
	key, kid := k["key"].(string), int64(k["id"].(float64))
	p := principalFor(uid, kid)

	if r := h.intercept(p, "c1", "gpt-6-sol"); r.Terminate {
		t.Fatalf("refused: %s", r.ResponseBody)
	}
	_, live := h.mgmt("GET", "live", nil)
	if live["running_total"].(float64) != 1 {
		t.Fatalf("live: %v", live)
	}
	usage := func(rid, model string) {
		rec, _ := json.Marshal(usageRecord{RequestID: rid, APIKey: p, Model: model, Generate: true, RequestedAt: time.Now(),
			Detail: usageDetail{InputTokens: 1_000_000, CachedTokens: 600_000, OutputTokens: 100_000, TotalTokens: 1_100_000}})
		h.call(methodUsageHandle, rec, nil)
	}
	usage("c1", "gpt-6-sol")     // 0.8 + 0.12 + 1.6 = 2.52
	usage("c2", "deepseek-v4.1") // unpriced: 0
	_, st := h.mgmt("GET", "state", nil)
	q := st["users"].([]any)[0].(map[string]any)["quota"].(map[string]any)
	if day := q["day"].(map[string]any); day["cost"].(float64) != 2.52 || q["total_cost"].(float64) != 2.52 {
		t.Fatalf("quota cost: %v", q)
	}
	if up := st["unpriced_models"].([]any); len(up) != 1 || up[0] != "deepseek-v4.1" {
		t.Fatalf("unpriced: %v", up)
	}
	if r := h.intercept(p, "c3", "gpt-6-sol"); !r.Terminate || r.StatusCode != 429 || !strings.Contains(string(r.ResponseBody), "金额") {
		t.Fatalf("daily cost limit: %+v %s", r, r.ResponseBody)
	}
	_, rep := h.mgmt("GET", "usage", nil)
	if rep["totals"].(map[string]any)["cost"].(float64) != 2.52 {
		t.Fatalf("usage cost: %v", rep["totals"])
	}

	// Pricing a model that had no price fills in its history right away.
	if _, r := h.mgmt("POST", "price/save", map[string]any{"model": "deepseek*", "input": 1}); len(r["backfilled_models"].([]any)) != 1 {
		t.Fatalf("manual price backfill: %v", r)
	}
	// Recompute with unchanged prices has nothing left to change.
	if s, r := h.mgmt("POST", "prices/recompute", map[string]any{}); s != 200 || r["updated"].(float64) != 0 {
		t.Fatalf("recompute: %d %v", s, r)
	}
	_, st = h.mgmt("GET", "state", nil)
	q = st["users"].([]any)[0].(map[string]any)["quota"].(map[string]any)
	if q["total_cost"].(float64) != 2.92 {
		t.Fatalf("after recompute: %v", q)
	}

	meReq, _ := json.Marshal(managementRequest{Method: "GET", Path: resourceBase + "me/data",
		Headers: http.Header{"Authorization": []string{"Bearer " + key}, "X-Real-Ip": []string{"1.2.3.4"}}})
	var me managementResponse
	h.call(methodManagementHandle, meReq, &me)
	var body map[string]any
	_ = json.Unmarshal(me.Body, &body)
	if me.StatusCode != 200 || body["currency_symbol"] != "$" || body["user"].(map[string]any)["daily_cost_limit"].(float64) != 2.5 {
		t.Fatalf("me: %d %s", me.StatusCode, me.Body)
	}
	if strings.Contains(string(me.Body), `"provider":"`) && !strings.Contains(string(me.Body), `"provider":""`) {
		t.Fatalf("provider leaked to self-service: %s", me.Body)
	}

	// Failed lookups get rate limited per address.
	bad, _ := json.Marshal(managementRequest{Method: "GET", Path: resourceBase + "me/data",
		Headers: http.Header{"Authorization": []string{"Bearer sk-cu-nope"}, "X-Real-Ip": []string{"5.6.7.8"}}})
	var last managementResponse
	for i := 0; i < 12; i++ {
		h.call(methodManagementHandle, bad, &last)
	}
	if last.StatusCode != 429 {
		t.Fatalf("self-service brute force not limited: %d", last.StatusCode)
	}
}

// A database created by v0.1.0 gains the new columns without losing data.
func TestMigrateFromV010(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate a v0.1.0 users table without the new columns.
	for _, q := range []string{`DROP TABLE users`, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		note TEXT NOT NULL DEFAULT '', group_id INTEGER NOT NULL DEFAULT 0, extra_models TEXT NOT NULL DEFAULT '[]',
		disabled INTEGER NOT NULL DEFAULT 0, expires_at INTEGER NOT NULL DEFAULT 0, daily_tokens INTEGER NOT NULL DEFAULT 0,
		monthly_tokens INTEGER NOT NULL DEFAULT 0, daily_requests INTEGER NOT NULL DEFAULT 0,
		monthly_requests INTEGER NOT NULL DEFAULT 0, max_concurrency INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO users (id, name, max_concurrency) VALUES (3, 'old', 2)`} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.close()
	st, err = openStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.close()
	_, users, _, err := st.loadAll()
	if err != nil || len(users) != 1 || users[0].MaxConcurrency != 2 || users[0].RPMLimit != 0 {
		t.Fatalf("migrated users: %+v %v", users, err)
	}
}
