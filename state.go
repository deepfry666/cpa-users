package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// principalPrefix marks principals issued by this plugin. CPA hands the
// principal back as UsageRecord.APIKey and hashes it into the caller scope, so
// encoding the user and key IDs here is how every later hook finds the owner.
const principalPrefix = "cpa-users/"

func principalFor(userID, keyID int64) string {
	return fmt.Sprintf("%s%d/%d", principalPrefix, userID, keyID)
}

func parsePrincipal(p string) (userID, keyID int64, ok bool) {
	rest, found := strings.CutPrefix(p, principalPrefix)
	if !found {
		return 0, 0, false
	}
	u, k, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	uid, err1 := strconv.ParseInt(u, 10, 64)
	kid, err2 := strconv.ParseInt(k, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uid, kid, true
}

// callerScope mirrors CPA's session.CallerScope: the only caller identity
// request interceptors receive.
func callerScope(principal string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + principal))
	return hex.EncodeToString(sum[:])
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type counter struct {
	Tokens   int64 `json:"tokens"`
	Requests int64 `json:"requests"`
	Micros   int64 `json:"-"`
}

// counterView is a counter as the console and self-service page see it.
type counterView struct {
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
	Cost     float64 `json:"cost"`
}

func (c *counter) view() counterView {
	if c == nil {
		return counterView{}
	}
	return counterView{Tokens: c.Tokens, Requests: c.Requests, Cost: microsToMoney(c.Micros)}
}

type inflight struct {
	userID int64
	at     time.Time
}

// state is the in-memory view every request hook reads. Admin writes go to
// SQLite first and then rebuild the index.
type state struct {
	mu      sync.RWMutex
	groups  map[int64]*Group
	users   map[int64]*User
	keys    map[int64]*Key
	byHash  map[string]int64
	byScope map[string]int64
	allowed map[int64][]string // effective patterns per user
	prices  *priceTable
	revoked map[string]struct{} // caller scopes of keys removed since start

	cmu      sync.Mutex
	loc      *time.Location
	dayStart time.Time
	monStart time.Time
	day      map[int64]*counter
	month    map[int64]*counter
	total    map[int64]int64       // all-time spend in micros
	recent   map[int64][]time.Time // admissions in the last minute, oldest first
	running  map[int64]int
	flights  map[string]inflight
	lastUsed map[int64]int64

	modelsMu sync.Mutex
	models   map[string]struct{}
}

func newState() *state {
	return &state{
		groups: map[int64]*Group{}, users: map[int64]*User{}, keys: map[int64]*Key{},
		byHash: map[string]int64{}, byScope: map[string]int64{}, allowed: map[int64][]string{}, revoked: map[string]struct{}{},
		loc: time.Local, day: map[int64]*counter{}, month: map[int64]*counter{},
		total: map[int64]int64{}, recent: map[int64][]time.Time{}, prices: newPriceTable(nil),
		running: map[int64]int{}, flights: map[string]inflight{}, lastUsed: map[int64]int64{},
		models: map[string]struct{}{},
	}
}

func (s *state) rebuild(groups []Group, users []User, keys []Key) {
	g := make(map[int64]*Group, len(groups))
	for i := range groups {
		g[groups[i].ID] = &groups[i]
	}
	u := make(map[int64]*User, len(users))
	allowed := make(map[int64][]string, len(users))
	for i := range users {
		user := &users[i]
		u[user.ID] = user
		patterns := append([]string{}, user.ExtraModels...)
		if grp, ok := g[user.GroupID]; ok {
			patterns = append(patterns, grp.Models...)
		}
		allowed[user.ID] = normalizePatterns(patterns)
	}
	k := make(map[int64]*Key, len(keys))
	byHash := make(map[string]int64, len(keys))
	byScope := make(map[string]int64, len(keys))
	for i := range keys {
		key := &keys[i]
		if _, ok := u[key.UserID]; !ok {
			continue
		}
		k[key.ID] = key
		byHash[key.hash] = key.ID
		byScope[callerScope(principalFor(key.UserID, key.ID))] = key.ID
	}
	s.mu.Lock()
	for scope := range s.byScope {
		if _, still := byScope[scope]; !still {
			s.revoked[scope] = struct{}{}
		}
	}
	s.groups, s.users, s.keys, s.byHash, s.byScope, s.allowed = g, u, k, byHash, byScope, allowed
	s.mu.Unlock()
}

// revokedScope reports whether scope belonged to a key that has since been
// deleted (or whose user was deleted) while CPA was running.
func (s *state) revokedScope(scope string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.revoked[scope]
	return ok
}

// lookup resolves a key ID to copies of its key and user.
func (s *state) lookup(keyID int64) (Key, User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[keyID]
	if !ok {
		return Key{}, User{}, false
	}
	u, ok := s.users[k.UserID]
	if !ok {
		return Key{}, User{}, false
	}
	return *k, *u, true
}

func (s *state) keyByHash(hash string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byHash[hash]
	return id, ok
}

func (s *state) keyByScope(scope string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byScope[scope]
	return id, ok
}

func (s *state) patterns(userID int64) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowed[userID]
}

func (s *state) userName(userID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u, ok := s.users[userID]; ok {
		return u.Name
	}
	return ""
}

// userBlocked reports why a user may not make requests right now.
func userBlocked(u User, now time.Time) string {
	if u.Disabled {
		return "账号已停用"
	}
	if u.ExpiresAt > 0 && now.Unix() >= u.ExpiresAt {
		return "账号已过期"
	}
	return ""
}

// ---- quota windows ----

func startOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func startOfMonth(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
}

func (s *state) setPrices(rows []Price) {
	t := newPriceTable(rows)
	s.mu.Lock()
	s.prices = t
	s.mu.Unlock()
}

func (s *state) priceTable() *priceTable {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prices
}

// priceFor finds the price of the first model name that has one.
func (s *state) priceFor(models ...string) (Price, bool) {
	t := s.priceTable()
	for _, m := range models {
		if p, ok := t.lookup(m); ok {
			return p, true
		}
	}
	return Price{}, false
}

// seedCounters replaces the quota counters with totals loaded from storage.
// Hour buckets are UTC-aligned, so with a non-whole-hour timezone the first
// hour of a day may be counted into the previous day.
func (s *state) seedCounters(loc *time.Location, now time.Time, rows []hourlyRow, totals map[int64]int64) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.loc = loc
	s.dayStart, s.monStart = startOfDay(now, loc), startOfMonth(now, loc)
	s.day, s.month = map[int64]*counter{}, map[int64]*counter{}
	s.total = map[int64]int64{}
	for uid, m := range totals {
		s.total[uid] = m
	}
	for _, r := range rows {
		t := time.Unix(r.Hour, 0)
		if t.Before(s.monStart) {
			continue
		}
		addTo(s.month, r.UserID, r.Total, r.Requests, r.CostMicros)
		if !t.Before(s.dayStart) {
			addTo(s.day, r.UserID, r.Total, r.Requests, r.CostMicros)
		}
	}
}

func addTo(m map[int64]*counter, userID, tokens, requests, micros int64) {
	c := m[userID]
	if c == nil {
		c = &counter{}
		m[userID] = c
	}
	c.Tokens += tokens
	c.Requests += requests
	c.Micros += micros
}

// rollLocked starts new day/month windows when the clock has passed them.
func (s *state) rollLocked(now time.Time) {
	if d := startOfDay(now, s.loc); d.After(s.dayStart) {
		s.dayStart, s.day = d, map[int64]*counter{}
	}
	if m := startOfMonth(now, s.loc); m.After(s.monStart) {
		s.monStart, s.month = m, map[int64]*counter{}
	}
}

// addUsage counts one upstream attempt. Failed attempts count toward the
// request limits too: they still load the shared upstream accounts.
func (s *state) addUsage(userID int64, at time.Time, tokens, micros int64) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.rollLocked(time.Now())
	const reqs = 1
	s.total[userID] += micros
	if !at.Before(s.monStart) {
		addTo(s.month, userID, tokens, reqs, micros)
	}
	if !at.Before(s.dayStart) {
		addTo(s.day, userID, tokens, reqs, micros)
	}
}

// QuotaView is the usage-versus-limit snapshot for one user.
type QuotaView struct {
	Day        counterView `json:"day"`
	Month      counterView `json:"month"`
	TotalCost  float64     `json:"total_cost"`
	Running    int         `json:"running"`
	RPM        int         `json:"rpm"`
	DayReset   int64       `json:"day_reset"`
	MonthReset int64       `json:"month_reset"`

	dayMicros, monthMicros, totalMicros int64
}

func (s *state) quota(userID int64, now time.Time) QuotaView {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.rollLocked(now)
	v := QuotaView{Running: s.running[userID], RPM: s.rpmLocked(userID, now)}
	v.Day, v.Month = s.day[userID].view(), s.month[userID].view()
	if c := s.day[userID]; c != nil {
		v.dayMicros = c.Micros
	}
	if c := s.month[userID]; c != nil {
		v.monthMicros = c.Micros
	}
	v.totalMicros = s.total[userID]
	v.TotalCost = microsToMoney(v.totalMicros)
	v.DayReset = s.dayStart.AddDate(0, 0, 1).Unix()
	v.MonthReset = s.monStart.AddDate(0, 1, 0).Unix()
	return v
}

func fmtMoney(v float64) string {
	if v >= 1 || v == 0 {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.4f", v)
}

// quotaExceeded returns a reason and reset time when any limit is used up.
// Limits are checked before a request, so one request can overshoot; the
// next one is refused. A zero reset time means the limit never resets.
func quotaExceeded(u User, q QuotaView) (string, time.Time) {
	over := func(limit float64, used int64) bool { return limit > 0 && used >= moneyToMicros(limit) }
	switch {
	case over(u.TotalCostLimit, q.totalMicros):
		return fmt.Sprintf("累计金额额度已用完（%s/%s）", fmtMoney(q.TotalCost), fmtMoney(u.TotalCostLimit)), time.Time{}
	case over(u.DailyCostLimit, q.dayMicros):
		return fmt.Sprintf("今日金额额度已用完（%s/%s）", fmtMoney(q.Day.Cost), fmtMoney(u.DailyCostLimit)), time.Unix(q.DayReset, 0)
	case over(u.MonthlyCostLimit, q.monthMicros):
		return fmt.Sprintf("本月金额额度已用完（%s/%s）", fmtMoney(q.Month.Cost), fmtMoney(u.MonthlyCostLimit)), time.Unix(q.MonthReset, 0)
	case u.DailyRequests > 0 && q.Day.Requests >= u.DailyRequests:
		return fmt.Sprintf("今日请求次数已用完（%d/%d）", q.Day.Requests, u.DailyRequests), time.Unix(q.DayReset, 0)
	case u.DailyTokens > 0 && q.Day.Tokens >= u.DailyTokens:
		return fmt.Sprintf("今日 Token 额度已用完（%d/%d）", q.Day.Tokens, u.DailyTokens), time.Unix(q.DayReset, 0)
	case u.MonthlyRequests > 0 && q.Month.Requests >= u.MonthlyRequests:
		return fmt.Sprintf("本月请求次数已用完（%d/%d）", q.Month.Requests, u.MonthlyRequests), time.Unix(q.MonthReset, 0)
	case u.MonthlyTokens > 0 && q.Month.Tokens >= u.MonthlyTokens:
		return fmt.Sprintf("本月 Token 额度已用完（%d/%d）", q.Month.Tokens, u.MonthlyTokens), time.Unix(q.MonthReset, 0)
	}
	return "", time.Time{}
}

// ---- concurrency ----

// staleFlight frees a slot whose completion never arrived. CPA normally sends
// request.complete; this only guards against a lost callback, so it is long
// enough for multi-hour agent streams.
const staleFlight = 3 * time.Hour

// rpmLocked counts admissions in the minute before now, dropping older ones.
func (s *state) rpmLocked(userID int64, now time.Time) int {
	list := s.recent[userID]
	cut := 0
	for cut < len(list) && now.Sub(list[cut]) >= time.Minute {
		cut++
	}
	if cut > 0 {
		list = list[cut:]
		if len(list) == 0 {
			delete(s.recent, userID)
		} else {
			s.recent[userID] = list
		}
	}
	return len(list)
}

type admitResult int

const (
	admitted admitResult = iota
	deniedConcurrency
	deniedRPM
)

// acquire admits requestID for userID unless the user is at the concurrency
// or per-minute limit. Repeated calls for the same request are idempotent.
// On an RPM denial retryAt says when the oldest admission leaves the window.
func (s *state) acquire(requestID string, userID int64, max, rpm int, now time.Time) (admitResult, time.Time) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if f, ok := s.flights[requestID]; ok && requestID != "" && f.userID == userID {
		return admitted, time.Time{}
	}
	for id, f := range s.flights {
		if now.Sub(f.at) > staleFlight {
			delete(s.flights, id)
			if s.running[f.userID] > 0 {
				s.running[f.userID]--
			}
		}
	}
	if max > 0 && s.running[userID] >= max {
		return deniedConcurrency, now.Add(2 * time.Second)
	}
	if n := s.rpmLocked(userID, now); rpm > 0 && n >= rpm {
		// The window drops below the limit once the (n-rpm+1)-th oldest
		// admission leaves it; with a freshly lowered limit that is not the oldest.
		return deniedRPM, s.recent[userID][n-rpm].Add(time.Minute)
	}
	s.recent[userID] = append(s.recent[userID], now)
	if requestID == "" {
		// Without an ID there is nothing to release later, so do not hold a slot.
		return admitted, time.Time{}
	}
	s.flights[requestID] = inflight{userID: userID, at: now}
	s.running[userID]++
	return admitted, time.Time{}
}

// LiveUser is one user's in-flight snapshot for the console's live view.
type LiveUser struct {
	ID              int64 `json:"id"`
	Running         int   `json:"running"`
	Max             int   `json:"max"`
	RPM             int   `json:"rpm"`
	RPMLimit        int   `json:"rpm_limit"`
	OldestStartedAt int64 `json:"oldest_started_at"`
}

func (s *state) live(now time.Time) ([]LiveUser, int) {
	_, users, _ := s.snapshot()
	s.cmu.Lock()
	defer s.cmu.Unlock()
	oldest := map[int64]time.Time{}
	for _, f := range s.flights {
		if o, ok := oldest[f.userID]; !ok || f.at.Before(o) {
			oldest[f.userID] = f.at
		}
	}
	out := make([]LiveUser, 0, len(users))
	total := 0
	for _, u := range users {
		lu := LiveUser{ID: u.ID, Running: s.running[u.ID], Max: u.MaxConcurrency, RPM: s.rpmLocked(u.ID, now), RPMLimit: u.RPMLimit}
		if o, ok := oldest[u.ID]; ok {
			lu.OldestStartedAt = o.Unix()
		}
		total += lu.Running
		out = append(out, lu)
	}
	return out, total
}

func (s *state) release(requestID string) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	f, ok := s.flights[requestID]
	if !ok {
		return
	}
	delete(s.flights, requestID)
	if s.running[f.userID] > 0 {
		s.running[f.userID]--
	}
}

// ---- key last-used tracking ----

func (s *state) touch(keyID int64, at time.Time) {
	s.cmu.Lock()
	s.lastUsed[keyID] = at.Unix()
	s.cmu.Unlock()
	s.mu.Lock()
	if k, ok := s.keys[keyID]; ok && at.Unix() > k.LastUsedAt {
		k.LastUsedAt = at.Unix()
	}
	s.mu.Unlock()
}

func (s *state) drainTouches() map[int64]int64 {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	out := s.lastUsed
	s.lastUsed = map[int64]int64{}
	return out
}

// ---- discovered models ----

// noteModels remembers model IDs seen in catalogs and usage, so the console
// can offer them as checkboxes. It reports whether anything new was added.
func (s *state) noteModels(ids ...string) bool {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	added := false
	for _, id := range ids {
		id = strings.TrimSpace(strings.TrimPrefix(id, "models/"))
		if id == "" {
			continue
		}
		if _, ok := s.models[id]; !ok {
			s.models[id] = struct{}{}
			added = true
		}
	}
	return added
}

func (s *state) knownModels() []string {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	out := make([]string, 0, len(s.models))
	for id := range s.models {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// snapshot copies the index for the console.
func (s *state) snapshot() ([]Group, []User, []Key) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	groups := make([]Group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, *g)
	}
	users := make([]User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, *u)
	}
	keys := make([]Key, 0, len(s.keys))
	for _, k := range s.keys {
		keys = append(keys, *k)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return groups, users, keys
}
