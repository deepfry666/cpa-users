package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// extractKey finds a client key the same way CPA's native provider does:
// Authorization Bearer, x-goog-api-key, x-api-key, then ?key= / ?auth_token=.
func extractKey(headers http.Header, query url.Values) string {
	if v := strings.TrimSpace(headers.Get("Authorization")); v != "" {
		if scheme, token, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
		return v
	}
	for _, h := range []string{"X-Goog-Api-Key", "X-Api-Key"} {
		if v := strings.TrimSpace(headers.Get(h)); v != "" {
			return v
		}
	}
	for _, q := range []string{"key", "auth_token"} {
		if v := strings.TrimSpace(query.Get(q)); v != "" {
			return v
		}
	}
	return ""
}

// ---- frontend auth ----

func (a *App) authenticate(payload []byte) ([]byte, error) {
	var req frontendAuthRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode auth request: %w", err)
	}
	notHandled := frontendAuthResponse{}
	if !a.ready() {
		return okEnvelope(notHandled)
	}
	key := extractKey(req.Headers, req.Query)
	if key == "" || !strings.HasPrefix(key, a.config().KeyPrefix) {
		return okEnvelope(notHandled)
	}
	keyID, ok := a.state.keyByHash(hashKey(key))
	if !ok {
		// Unknown keys fall through so CPA answers with its usual 401.
		return okEnvelope(notHandled)
	}
	k, u, ok := a.state.lookup(keyID)
	if !ok {
		return okEnvelope(notHandled)
	}
	if k.Disabled {
		return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusUnauthorized, DenyMessage: "API Key 已停用"})
	}
	if reason := userBlocked(u, time.Now()); reason != "" {
		return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusForbidden, DenyMessage: reason})
	}
	switch routeFor(req.Method, req.Path) {
	case routeDenied:
		return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusForbidden, DenyMessage: "此接口不对分发的 Key 开放"})
	case routeImages:
		if !imagesAllowed(a.state.patterns(u.ID)) {
			return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusForbidden, DenyMessage: "当前账号未开通生图"})
		}
	case routeCatalog:
		// The catalog filter only sees request headers, so a key passed as a
		// query parameter would get the full, unfiltered list.
		if headerKey := extractKey(req.Headers, nil); headerKey != key {
			return okEnvelope(frontendAuthResponse{DenyStatusCode: http.StatusBadRequest, DenyMessage: "请通过请求头传递 API Key"})
		}
	}
	return okEnvelope(frontendAuthResponse{
		Authenticated: true,
		Principal:     principalFor(u.ID, k.ID),
		Metadata:      map[string]string{"user": u.Name, "key": k.Name},
	})
}

// ---- admission ----

func metaString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (a *App) interceptBefore(payload []byte) ([]byte, error) {
	var req requestInterceptRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode intercept request: %w", err)
	}
	pass := requestInterceptResponse{}
	if !a.ready() || metaString(req.Metadata, metaSource) == sourceHostModelCallback {
		return okEnvelope(pass)
	}
	scope := req.CallerScope
	if scope == "" {
		scope = metaString(req.Metadata, metaCallerScope)
	}
	keyID, ok := a.state.keyByScope(scope)
	if !ok {
		if a.state.revokedScope(scope) {
			// Authenticated before the key was deleted; do not let it through unmetered.
			return okEnvelope(refuse(req.SourceFormat, http.StatusUnauthorized, "invalid_api_key", "API Key 已删除", time.Time{}))
		}
		return okEnvelope(pass) // native CPA key or anonymous: not ours
	}
	now := time.Now()
	k, u, ok := a.state.lookup(keyID)
	if !ok || k.Disabled {
		return okEnvelope(refuse(req.SourceFormat, http.StatusUnauthorized, "invalid_api_key", "API Key 已停用或已删除", time.Time{}))
	}
	if reason := userBlocked(u, now); reason != "" {
		return okEnvelope(refuse(req.SourceFormat, http.StatusForbidden, "account_unavailable", reason, time.Time{}))
	}

	// Both the requested name and the resolved one must be allowed, so an
	// allowed alias cannot reach a model outside the list. Requests without
	// any model name are refused rather than guessed at.
	patterns := a.state.patterns(u.ID)
	names := []string{}
	for _, m := range []string{req.RequestedModel, req.Model} {
		if strings.TrimSpace(m) != "" {
			names = append(names, m)
		}
	}
	if len(names) == 0 {
		return okEnvelope(refuse(req.SourceFormat, http.StatusBadRequest, "model_required", "请求里没有指定模型", time.Time{}))
	}
	for _, m := range names {
		if !modelAllowed(patterns, m) {
			msg := fmt.Sprintf("当前账号（%s）无权使用模型 %s", u.Name, m)
			return okEnvelope(refuse(req.SourceFormat, http.StatusForbidden, "model_not_allowed", msg, time.Time{}))
		}
	}

	// The generate flag comes from the client's JSON, so it is not trusted:
	// every request is checked against quotas, concurrency and RPM.
	if reason, reset := quotaExceeded(u, a.state.quota(u.ID, now)); reason != "" {
		return okEnvelope(refuse(req.SourceFormat, http.StatusTooManyRequests, "quota_exceeded", reason, reset))
	}
	switch res, retryAt := a.state.acquire(req.RequestID, u.ID, u.MaxConcurrency, u.RPMLimit, now); res {
	case deniedConcurrency:
		msg := fmt.Sprintf("并发请求已达上限（%d）", u.MaxConcurrency)
		return okEnvelope(refuse(req.SourceFormat, http.StatusTooManyRequests, "concurrency_limit", msg, retryAt))
	case deniedRPM:
		msg := fmt.Sprintf("每分钟请求次数已达上限（%d）", u.RPMLimit)
		return okEnvelope(refuse(req.SourceFormat, http.StatusTooManyRequests, "rate_limit", msg, retryAt))
	}
	return okEnvelope(pass)
}

func (a *App) complete(payload []byte) ([]byte, error) {
	var c requestCompletion
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("decode completion: %w", err)
	}
	a.state.release(c.RequestID)
	return okEnvelope(map[string]any{})
}

// refuse ends a request without reaching an upstream, shaped like the error
// format the client speaks so SDKs surface the message.
func refuse(sourceFormat string, status int, code, message string, retryAt time.Time) requestInterceptResponse {
	var body any
	switch f := strings.ToLower(sourceFormat); {
	case strings.Contains(f, "claude"):
		typ := "permission_error"
		switch status {
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		case http.StatusUnauthorized:
			typ = "authentication_error"
		}
		body = map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}}
	case strings.Contains(f, "gemini"):
		st := "PERMISSION_DENIED"
		switch status {
		case http.StatusTooManyRequests:
			st = "RESOURCE_EXHAUSTED"
		case http.StatusUnauthorized:
			st = "UNAUTHENTICATED"
		}
		body = map[string]any{"error": map[string]any{"code": status, "message": message, "status": st}}
	default:
		typ := "permission_error"
		switch status {
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		case http.StatusUnauthorized:
			typ = "authentication_error"
		}
		body = map[string]any{"error": map[string]any{"message": message, "type": typ, "code": code}}
	}
	raw, _ := json.Marshal(body)
	headers := http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
	if !retryAt.IsZero() {
		secs := int(math.Ceil(time.Until(retryAt).Seconds()))
		if secs < 1 {
			secs = 1
		}
		headers.Set("Retry-After", strconv.Itoa(secs))
	}
	return requestInterceptResponse{Terminate: true, StatusCode: status, ResponseHeaders: headers, ResponseBody: raw}
}

// ---- model catalog filtering ----

// interceptResponse sees every successful non-streaming response. Only model
// catalogs carry no model name, so everything else returns immediately.
func (a *App) interceptResponse(payload []byte) ([]byte, error) {
	var head struct {
		Model          string `json:"Model"`
		RequestedModel string `json:"RequestedModel"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		return nil, fmt.Errorf("decode response intercept: %w", err)
	}
	if head.Model != "" || head.RequestedModel != "" || !a.ready() {
		return okEnvelope(responseInterceptResponse{})
	}
	var req responseInterceptRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode response intercept: %w", err)
	}
	body := a.filterCatalog(req.RequestHeaders, req.Body)
	return okEnvelope(responseInterceptResponse{Body: body})
}

// filterCatalog returns a trimmed catalog for plugin keys, or nil to leave
// the response untouched.
func (a *App) filterCatalog(headers http.Header, body []byte) []byte {
	catalog, ok := parseModelCatalog(body)
	if !ok {
		return nil
	}
	if a.state.noteModels(catalog.ids...) {
		a.markModelsDirty()
	}
	cfg := a.config()
	if !cfg.filterModels() {
		return nil
	}
	key := extractKey(headers, nil)
	if key == "" || !strings.HasPrefix(key, cfg.KeyPrefix) {
		return nil
	}
	keyID, ok := a.state.keyByHash(hashKey(key))
	if !ok {
		return nil
	}
	_, u, ok := a.state.lookup(keyID)
	if !ok {
		return nil
	}
	patterns := a.state.patterns(u.ID)
	out, _ := catalog.filter(func(id string) bool { return modelAllowed(patterns, id) })
	return out
}

// ---- usage ----

func (a *App) handleUsage(payload []byte) ([]byte, error) {
	var rec usageRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	st := a.db()
	if st == nil || !a.config().Enabled {
		return okEnvelope(map[string]any{})
	}
	// Hosts normalise omitted Generate to true, so false really means a probe.
	if !rec.Generate && !rec.Failed && rec.Detail.TotalTokens == 0 && rec.Detail.InputTokens == 0 {
		return okEnvelope(map[string]any{})
	}
	// Recomputing costs reseeds the counters; hold it off while this record
	// moves both the counters and the database.
	a.acctMu.Lock()
	defer a.acctMu.Unlock()
	userID, keyID, _ := parsePrincipal(rec.APIKey) // 0/0 = native key or anonymous
	model := rec.Alias
	if model == "" {
		model = rec.Model
	}
	at := rec.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	tu := normalizeUsage(rec.Detail)
	total := tu.Total
	row := usageRow{
		At: at, UserID: userID, KeyID: keyID, Model: model, Provider: rec.Provider, RequestID: rec.RequestID,
		Stream: rec.Stream, Failed: rec.Failed, Status: rec.Failure.StatusCode,
		Input: tu.Input, Output: tu.Output, Reasoning: tu.Reasoning, Cached: tu.Cached, Total: total,
		LatencyMs: rec.Latency.Milliseconds(), TTFTMs: rec.TTFT.Milliseconds(),
	}
	if !rec.Failed {
		tu.Successes = 1
	}
	if p, ok := a.state.priceFor(model, rec.Model); ok {
		row.CostMicros = costMicros(p, tu)
	}
	if rec.Failed {
		row.Error = truncate(rec.Failure.Body, 300)
		if row.Status == 0 {
			row.Status = http.StatusBadGateway
		}
	} else {
		row.Status = http.StatusOK
	}
	// Counters move first so a storage error cannot hand out free usage.
	if userID != 0 {
		a.state.addUsage(userID, at, total, row.CostMicros)
		a.state.touch(keyID, at)
	}
	if err := st.recordUsage(row); err != nil {
		return nil, fmt.Errorf("record usage: %w", err)
	}
	if !rec.Failed && a.state.noteModels(model) {
		a.markModelsDirty()
	}
	return okEnvelope(map[string]any{})
}
