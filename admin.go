package main

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/admin.html
var adminHTML []byte

//go:embed web/me.html
var meHTML []byte

const (
	mgmtBase     = "/v0/management/" + PluginID + "/"
	resourceBase = "/v0/resource/plugins/" + PluginID + "/"
)

type route struct {
	method, path, desc string
	handler            func(a *App, req managementRequest) managementResponse
}

var managementRoutes = []route{
	{http.MethodGet, "state", "用户、分组、Key 与额度快照", (*App).apiState},
	{http.MethodPost, "group/save", "新建或修改分组", (*App).apiSaveGroup},
	{http.MethodPost, "group/delete", "删除分组", (*App).apiDeleteGroup},
	{http.MethodPost, "user/save", "新建或修改用户", (*App).apiSaveUser},
	{http.MethodPost, "user/delete", "删除用户及其 Key", (*App).apiDeleteUser},
	{http.MethodPost, "key/create", "为用户签发 Key", (*App).apiCreateKey},
	{http.MethodPost, "key/update", "重命名或停用 Key", (*App).apiUpdateKey},
	{http.MethodPost, "key/delete", "删除 Key", (*App).apiDeleteKey},
	{http.MethodGet, "usage", "用量统计", (*App).apiUsage},
	{http.MethodGet, "logs", "请求明细", (*App).apiLogs},
	{http.MethodPost, "models/refresh", "从 CPA 凭证刷新可选模型", (*App).apiRefreshModels},
	{http.MethodGet, "live", "实时并发与每分钟请求数", (*App).apiLive},
	{http.MethodPost, "price/save", "新建或修改模型价格", (*App).apiSavePrice},
	{http.MethodPost, "price/delete", "删除模型价格", (*App).apiDeletePrice},
	{http.MethodPost, "prices/recompute", "按当前价格重算历史金额", (*App).apiRecomputeCosts},
}

func (a *App) managementRegistration() managementRegistrationResponse {
	resp := managementRegistrationResponse{
		Resources: []resourceRoute{
			{Path: "/ui", Menu: "用户分发", Description: "管理用户、可用模型、额度与用量"},
			{Path: "/me", Description: "用户自助查询页"},
			{Path: "/me/data", Description: "用户自助查询数据"},
		},
	}
	for _, r := range managementRoutes {
		resp.Routes = append(resp.Routes, managementRoute{Method: r.method, Path: "/" + PluginID + "/" + r.path, Description: r.desc})
	}
	return resp
}

func (a *App) handleManagement(payload []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	var resp managementResponse
	switch {
	case strings.HasPrefix(req.Path, resourceBase):
		resp = a.handleResource(req, strings.TrimPrefix(req.Path, resourceBase))
	case strings.HasPrefix(req.Path, mgmtBase):
		resp = a.handleAPI(req, strings.TrimPrefix(req.Path, mgmtBase))
	default:
		resp = jsonError(http.StatusNotFound, "not found")
	}
	return okEnvelope(resp)
}

func (a *App) handleAPI(req managementRequest, path string) managementResponse {
	for _, r := range managementRoutes {
		if r.path == path && strings.EqualFold(r.method, req.Method) {
			if a.db() == nil && path != "state" {
				return jsonError(http.StatusServiceUnavailable, "插件存储不可用："+a.storageError())
			}
			if r.method == http.MethodPost {
				a.writeMu.Lock()
				defer a.writeMu.Unlock()
			}
			return r.handler(a, req)
		}
	}
	return jsonError(http.StatusNotFound, "unknown route "+req.Method+" "+path)
}

func (a *App) handleResource(req managementRequest, path string) managementResponse {
	switch path {
	case "ui":
		return htmlResponse(adminHTML)
	case "me":
		return htmlResponse(meHTML)
	case "me/data":
		return a.apiMe(req)
	}
	return jsonError(http.StatusNotFound, "not found")
}

func (a *App) storageError() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case !a.cfg.Enabled:
		return "插件未启用"
	case a.initErr != "":
		return a.initErr
	default:
		return "未初始化"
	}
}

// ---- response helpers ----

func jsonResponse(status int, v any) managementResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       raw,
	}
}

func jsonError(status int, message string) managementResponse {
	return jsonResponse(status, map[string]string{"error": message})
}

func htmlResponse(body []byte) managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"text/html; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	}
}

func decodeBody(req managementRequest, v any) error {
	if len(req.Body) == 0 {
		return errors.New("请求体为空")
	}
	if err := json.Unmarshal(req.Body, v); err != nil {
		return fmt.Errorf("请求体格式错误：%w", err)
	}
	return nil
}

func saveError(err error) managementResponse {
	switch {
	case errors.Is(err, errDuplicateName):
		return jsonError(http.StatusConflict, err.Error())
	case errors.Is(err, sql.ErrNoRows):
		return jsonError(http.StatusNotFound, "记录不存在")
	default:
		return jsonError(http.StatusInternalServerError, err.Error())
	}
}

// ---- state ----

type userView struct {
	User
	Allowed  []string  `json:"allowed"`
	Quota    QuotaView `json:"quota"`
	KeyCount int       `json:"key_count"`
}

func (a *App) apiState(_ managementRequest) managementResponse {
	cfg := a.config()
	groups, users, keys := a.state.snapshot()
	now := time.Now()
	keyCount := map[int64]int{}
	for _, k := range keys {
		keyCount[k.UserID]++
	}
	views := make([]userView, 0, len(users))
	runningTotal := 0
	for _, u := range users {
		q := a.state.quota(u.ID, now)
		runningTotal += q.Running
		views = append(views, userView{User: u, Allowed: a.state.patterns(u.ID), Quota: q, KeyCount: keyCount[u.ID]})
	}
	prices := a.state.priceTable()
	unpriced := []string{}
	if st := a.db(); st != nil {
		if used, err := st.usedModels(now.AddDate(0, 0, -30)); err == nil {
			for _, m := range used {
				if _, ok := prices.lookup(m); !ok {
					unpriced = append(unpriced, m)
				}
			}
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"version":         PluginVersion,
		"ready":           a.db() != nil,
		"error":           map[bool]string{true: "", false: a.storageError()}[a.db() != nil],
		"groups":          groups,
		"users":           views,
		"keys":            keys,
		"models":          a.state.knownModels(),
		"prices":          prices.rows,
		"unpriced_models": unpriced,
		"running_total":   runningTotal,
		"now":             now.Unix(),
		"timezone":        cfg.Timezone,
		"config": map[string]any{
			"key_prefix":        cfg.KeyPrefix,
			"filter_model_list": cfg.filterModels(),
			"db_path":           cfg.DBPath,
			"log_retention":     cfg.LogRetentionDays,
			"public_base_url":   cfg.PublicBaseURL,
			"currency_symbol":   cfg.CurrencySymbol,
		},
	})
}

// ---- groups ----

func (a *App) apiSaveGroup(req managementRequest) managementResponse {
	var g Group
	if err := decodeBody(req, &g); err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return jsonError(http.StatusBadRequest, "分组名称不能为空")
	}
	if len([]rune(g.Name)) > 64 || len([]rune(g.Note)) > 500 {
		return jsonError(http.StatusBadRequest, "分组名称最多 64 字，备注最多 500 字")
	}
	if msg := checkPatterns(g.Models); msg != "" {
		return jsonError(http.StatusBadRequest, msg)
	}
	id, err := a.db().saveGroup(g)
	if err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"id": id})
}

func (a *App) apiDeleteGroup(req managementRequest) managementResponse {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decodeBody(req, &body); err != nil || body.ID == 0 {
		return jsonError(http.StatusBadRequest, "缺少分组 id")
	}
	if err := a.db().deleteGroup(body.ID); err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"ok": true})
}

// checkPatterns bounds model allowlists so one save cannot bloat every request.
func checkPatterns(list []string) string {
	if len(list) > 500 {
		return "模型规则最多 500 条"
	}
	for _, p := range list {
		if len(p) > 200 {
			return "单条模型规则最多 200 个字符"
		}
	}
	return ""
}

// ---- users ----

func (a *App) apiSaveUser(req managementRequest) managementResponse {
	var u User
	if err := decodeBody(req, &u); err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" {
		return jsonError(http.StatusBadRequest, "用户名不能为空")
	}
	if len([]rune(u.Name)) > 64 || len([]rune(u.Note)) > 500 {
		return jsonError(http.StatusBadRequest, "用户名最多 64 字，备注最多 500 字")
	}
	if u.DailyTokens < 0 || u.MonthlyTokens < 0 || u.DailyRequests < 0 || u.MonthlyRequests < 0 || u.MaxConcurrency < 0 ||
		u.RPMLimit < 0 || u.ExpiresAt < 0 {
		return jsonError(http.StatusBadRequest, "额度不能为负数")
	}
	if !validMoney(u.DailyCostLimit) || !validMoney(u.MonthlyCostLimit) || !validMoney(u.TotalCostLimit) {
		return jsonError(http.StatusBadRequest, "金额额度必须是 0 到 10 亿之间的数字")
	}
	if msg := checkPatterns(u.ExtraModels); msg != "" {
		return jsonError(http.StatusBadRequest, msg)
	}
	id, err := a.db().saveUser(u)
	if err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"id": id})
}

func (a *App) apiDeleteUser(req managementRequest) managementResponse {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decodeBody(req, &body); err != nil || body.ID == 0 {
		return jsonError(http.StatusBadRequest, "缺少用户 id")
	}
	if err := a.db().deleteUser(body.ID); err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"ok": true})
}

// ---- keys ----

const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func newKey(prefix string) (string, error) {
	var b strings.Builder
	b.WriteString(prefix)
	max := big.NewInt(int64(len(keyAlphabet)))
	for i := 0; i < 40; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(keyAlphabet[n.Int64()])
	}
	return b.String(), nil
}

func keyHint(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:10] + "…" + key[len(key)-4:]
}

func (a *App) apiCreateKey(req managementRequest) managementResponse {
	var body struct {
		UserID int64  `json:"user_id"`
		Name   string `json:"name"`
	}
	if err := decodeBody(req, &body); err != nil || body.UserID == 0 {
		return jsonError(http.StatusBadRequest, "缺少用户 id")
	}
	if len([]rune(body.Name)) > 64 {
		return jsonError(http.StatusBadRequest, "Key 名称最多 64 字")
	}
	if a.state.userName(body.UserID) == "" {
		return jsonError(http.StatusNotFound, "用户不存在")
	}
	key, err := newKey(a.config().KeyPrefix)
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	id, err := a.db().createKey(body.UserID, strings.TrimSpace(body.Name), hashKey(key), keyHint(key))
	if err != nil {
		return saveError(err)
	}
	// The full key is returned once; only its hash is stored.
	return a.afterWrite(map[string]any{"id": id, "key": key})
}

func (a *App) apiUpdateKey(req managementRequest) managementResponse {
	var body struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Disabled bool   `json:"disabled"`
	}
	if err := decodeBody(req, &body); err != nil || body.ID == 0 {
		return jsonError(http.StatusBadRequest, "缺少 Key id")
	}
	if err := a.db().updateKey(body.ID, strings.TrimSpace(body.Name), body.Disabled); err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"ok": true})
}

func (a *App) apiDeleteKey(req managementRequest) managementResponse {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decodeBody(req, &body); err != nil || body.ID == 0 {
		return jsonError(http.StatusBadRequest, "缺少 Key id")
	}
	if err := a.db().deleteKey(body.ID); err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"ok": true})
}

func (a *App) afterWrite(result map[string]any) managementResponse {
	if err := a.reload(); err != nil {
		return jsonError(http.StatusInternalServerError, "已保存，但刷新内存索引失败："+err.Error())
	}
	return jsonResponse(http.StatusOK, result)
}

// ---- usage ----

type usageTotals struct {
	Requests  int64   `json:"requests"`
	Failed    int64   `json:"failed"`
	Input     int64   `json:"input_tokens"`
	Output    int64   `json:"output_tokens"`
	Reasoning int64   `json:"reasoning_tokens"`
	Cached    int64   `json:"cached_tokens"`
	Total     int64   `json:"total_tokens"`
	Micros    int64   `json:"-"`
	Cost      float64 `json:"cost"`
}

func (t *usageTotals) add(r hourlyRow) {
	t.Micros += r.CostMicros
	t.Cost = microsToMoney(t.Micros)
	t.Requests += r.Requests
	t.Failed += r.Failed
	t.Input += r.Input
	t.Output += r.Output
	t.Reasoning += r.Reasoning
	t.Cached += r.Cached
	t.Total += r.Total
}

type usageReport struct {
	Days    []string                `json:"days"`
	Totals  usageTotals             `json:"totals"`
	Daily   []usageTotals           `json:"daily"`
	ByUser  map[string]*usageTotals `json:"by_user"`
	ByKey   map[string]*usageTotals `json:"by_key"`
	ByModel map[string]*usageTotals `json:"by_model"`
	// UserDaily[userID][dayIndex] holds per-user daily totals for charts.
	UserDaily map[string][]usageTotals `json:"user_daily"`
}

// buildUsage aggregates hourly rows into local-day buckets over the last n days.
func buildUsage(rows []hourlyRow, from time.Time, days int, loc *time.Location) usageReport {
	rep := usageReport{
		Days: make([]string, days), Daily: make([]usageTotals, days),
		ByUser: map[string]*usageTotals{}, ByKey: map[string]*usageTotals{}, ByModel: map[string]*usageTotals{},
		UserDaily: map[string][]usageTotals{},
	}
	index := map[string]int{}
	for i := 0; i < days; i++ {
		d := from.AddDate(0, 0, i).Format("2006-01-02")
		rep.Days[i] = d
		index[d] = i
	}
	bucket := func(m map[string]*usageTotals, k string) *usageTotals {
		t := m[k]
		if t == nil {
			t = &usageTotals{}
			m[k] = t
		}
		return t
	}
	for _, r := range rows {
		i, ok := index[time.Unix(r.Hour, 0).In(loc).Format("2006-01-02")]
		if !ok {
			continue
		}
		uid := strconv.FormatInt(r.UserID, 10)
		rep.Totals.add(r)
		rep.Daily[i].add(r)
		bucket(rep.ByUser, uid).add(r)
		bucket(rep.ByKey, strconv.FormatInt(r.KeyID, 10)).add(r)
		bucket(rep.ByModel, r.Model).add(r)
		series := rep.UserDaily[uid]
		if series == nil {
			series = make([]usageTotals, days)
			rep.UserDaily[uid] = series
		}
		series[i].add(r)
	}
	return rep
}

func queryInt(req managementRequest, name string, def, min, max int) int {
	v, err := strconv.Atoi(req.Query.Get(name))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func (a *App) usageFor(userID int64, days int) (usageReport, error) {
	loc := a.config().location()
	now := time.Now()
	from := startOfDay(now, loc).AddDate(0, 0, -(days - 1))
	rows, err := a.db().hourly(from, now.Add(time.Hour), userID)
	if err != nil {
		return usageReport{}, err
	}
	return buildUsage(rows, from, days, loc), nil
}

func (a *App) apiUsage(req managementRequest) managementResponse {
	days := queryInt(req, "days", 7, 1, 366)
	userID, _ := strconv.ParseInt(req.Query.Get("user_id"), 10, 64)
	rep, err := a.usageFor(userID, days)
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	return jsonResponse(http.StatusOK, rep)
}

func (a *App) apiLogs(req managementRequest) managementResponse {
	limit := queryInt(req, "limit", 200, 1, 1000)
	userID, _ := strconv.ParseInt(req.Query.Get("user_id"), 10, 64)
	logs, err := a.db().logs(limit, userID, req.Query.Get("failed") == "1")
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"logs": logs})
}

// apiRefreshModels asks CPA which models its credentials serve. Aliases only
// show up once a catalog request or usage record has passed through.
func (a *App) apiRefreshModels(req managementRequest) managementResponse {
	raw, err := callHost(methodHostAuthModels, map[string]any{"host_callback_id": req.HostCallbackID})
	if err != nil {
		return jsonError(http.StatusBadGateway, "读取 CPA 模型失败："+err.Error())
	}
	var resp hostAuthModelsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return jsonError(http.StatusBadGateway, err.Error())
	}
	var ids []string
	for _, c := range resp.Credentials {
		ids = append(ids, c.Models...)
	}
	added := a.state.noteModels(ids...)
	if added {
		a.saveModels()
	}
	return jsonResponse(http.StatusOK, map[string]any{"models": a.state.knownModels(), "added": added})
}

// ---- self-service ----

// apiMe serves the self-service page. It is a public resource route, so the
// caller proves identity with their own issued key.
func (a *App) apiMe(req managementRequest) managementResponse {
	if a.db() == nil {
		return jsonError(http.StatusServiceUnavailable, "服务暂不可用")
	}
	client := clientAddr(req.Headers)
	if !a.meLimit.allow(client, time.Now()) {
		resp := jsonError(http.StatusTooManyRequests, "查询太频繁，请稍后再试")
		resp.Headers.Set("Retry-After", "60")
		return resp
	}
	key := extractKey(req.Headers, nil)
	if key == "" || !strings.HasPrefix(key, a.config().KeyPrefix) {
		a.meLimit.fail(client, time.Now())
		return jsonError(http.StatusUnauthorized, "请输入有效的 API Key")
	}
	keyID, ok := a.state.keyByHash(hashKey(key))
	if !ok {
		a.meLimit.fail(client, time.Now())
		return jsonError(http.StatusUnauthorized, "API Key 无效")
	}
	k, u, ok := a.state.lookup(keyID)
	if !ok {
		return jsonError(http.StatusUnauthorized, "API Key 无效")
	}
	if k.Disabled {
		return jsonError(http.StatusUnauthorized, "API Key 已停用")
	}
	patterns := a.state.patterns(u.ID)
	available := []string{}
	prices := []Price{}
	table := a.state.priceTable()
	for _, m := range a.state.knownModels() {
		if modelAllowed(patterns, m) {
			available = append(available, m)
			if p, ok := table.lookup(m); ok {
				prices = append(prices, Price{Model: m, Input: p.Input, Cached: p.Cached, Output: p.Output, PerRequest: p.PerRequest})
			}
		}
	}
	sort.Strings(available)
	rep, err := a.usageFor(u.ID, 14)
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	logs, err := a.db().logs(30, u.ID, false)
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	for i := range logs {
		logs[i].Error = ""    // upstream error bodies stay admin-only
		logs[i].Provider = "" // so does which upstream served it
	}
	now := time.Now()
	return jsonResponse(http.StatusOK, map[string]any{
		"user": map[string]any{
			"name": u.Name, "expires_at": u.ExpiresAt, "disabled": u.Disabled,
			"daily_tokens": u.DailyTokens, "monthly_tokens": u.MonthlyTokens,
			"daily_requests": u.DailyRequests, "monthly_requests": u.MonthlyRequests,
			"max_concurrency": u.MaxConcurrency, "blocked": userBlocked(u, now),
			"daily_cost_limit": u.DailyCostLimit, "monthly_cost_limit": u.MonthlyCostLimit,
			"total_cost_limit": u.TotalCostLimit, "rpm_limit": u.RPMLimit,
		},
		"currency_symbol": a.config().CurrencySymbol,
		"prices":          prices,
		"key":             map[string]any{"name": k.Name, "hint": k.Hint, "disabled": k.Disabled},
		"patterns":        patterns,
		"available":       available,
		"quota":           a.state.quota(u.ID, now),
		"usage":           rep,
		"logs":            logs,
	})
}

// ---- live ----

func (a *App) apiLive(_ managementRequest) managementResponse {
	now := time.Now()
	users, total := a.state.live(now)
	return jsonResponse(http.StatusOK, map[string]any{"now": now.Unix(), "running_total": total, "users": users})
}

// ---- prices ----

func (a *App) apiSavePrice(req managementRequest) managementResponse {
	var body struct {
		Price
		OldModel string `json:"old_model"`
	}
	if err := decodeBody(req, &body); err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	p := body.Price
	p.Model = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p.Model), "models/")))
	p.Note = strings.TrimSpace(p.Note)
	if p.Model == "" || len(p.Model) > 200 {
		return jsonError(http.StatusBadRequest, "模型名不能为空，最多 200 个字符")
	}
	if len([]rune(p.Note)) > 200 {
		return jsonError(http.StatusBadRequest, "备注最多 200 字")
	}
	for _, v := range []float64{p.Input, p.Cached, p.Output, p.PerRequest} {
		if !validMoney(v) {
			return jsonError(http.StatusBadRequest, errBadPrice.Error())
		}
	}
	if err := a.db().savePrice(p, strings.TrimSpace(body.OldModel)); err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"ok": true})
}

func (a *App) apiDeletePrice(req managementRequest) managementResponse {
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeBody(req, &body); err != nil || strings.TrimSpace(body.Model) == "" {
		return jsonError(http.StatusBadRequest, "缺少模型名")
	}
	if err := a.db().deletePrice(strings.TrimSpace(body.Model)); err != nil {
		return saveError(err)
	}
	return a.afterWrite(map[string]any{"ok": true})
}

func (a *App) apiRecomputeCosts(_ managementRequest) managementResponse {
	a.acctMu.Lock()
	defer a.acctMu.Unlock()
	n, err := a.db().recomputeCosts(a.state.priceTable())
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	a.seedCountersLocked()
	return jsonResponse(http.StatusOK, map[string]any{"updated": n})
}

// ---- self-service rate limit ----

// clientAddr picks the caller's address from proxy headers; CPA passes the
// request headers to resource routes. Unknown callers share one bucket.
func clientAddr(h http.Header) string {
	if v := strings.TrimSpace(h.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := h.Get("X-Forwarded-For"); v != "" {
		first, _, _ := strings.Cut(v, ",")
		return strings.TrimSpace(first)
	}
	return "unknown"
}

// failLimiter blocks an address for a minute after too many failed key
// lookups on the self-service page, and caps total lookups per minute.
type failLimiter struct {
	mu    sync.Mutex
	seen  map[string]*limitBucket
	clean time.Time
}

type limitBucket struct {
	start         time.Time
	hits, failure int
}

const (
	meMaxPerMinute   = 60
	meMaxFailsMinute = 10
)

func (l *failLimiter) bucket(addr string, now time.Time) *limitBucket {
	if l.seen == nil {
		l.seen = map[string]*limitBucket{}
	}
	if now.Sub(l.clean) > 5*time.Minute {
		for k, b := range l.seen {
			if now.Sub(b.start) > time.Minute {
				delete(l.seen, k)
			}
		}
		l.clean = now
	}
	b := l.seen[addr]
	if b == nil || now.Sub(b.start) > time.Minute {
		b = &limitBucket{start: now}
		l.seen[addr] = b
	}
	return b
}

func (l *failLimiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(addr, now)
	b.hits++
	return b.hits <= meMaxPerMinute && b.failure < meMaxFailsMinute
}

func (l *failLimiter) fail(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bucket(addr, now).failure++
}
