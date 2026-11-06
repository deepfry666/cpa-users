package main

import (
	"encoding/json"
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
	if !s.acquire("r1", 1, 1, now) || !s.acquire("r1", 1, 1, now) {
		t.Fatal("same request must be admitted twice")
	}
	if s.acquire("r2", 1, 1, now) {
		t.Fatal("second request must be refused at max 1")
	}
	s.release("r1")
	s.release("r1")
	if !s.acquire("r2", 1, 1, now) {
		t.Fatal("slot not released")
	}
	if s.running[1] != 1 {
		t.Fatalf("running = %d", s.running[1])
	}
}

func TestQuotaRollsOverAtMidnight(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	s := newState()
	day1 := time.Date(2026, 11, 6, 23, 0, 0, 0, loc)
	s.seedCounters(loc, day1, nil)
	s.addUsage(7, day1, 500, true)
	if q := s.quota(7, day1); q.Day.Tokens != 500 || q.Month.Tokens != 500 {
		t.Fatalf("quota after usage: %+v", q)
	}
	day2 := day1.Add(2 * time.Hour)
	q := s.quota(7, day2)
	if q.Day.Tokens != 0 || q.Month.Tokens != 500 {
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
	cfg := "enabled: true\ndb_path: " + db + "\ntimezone: Asia/Shanghai\n"
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
