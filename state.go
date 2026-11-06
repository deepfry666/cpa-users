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

	cmu      sync.Mutex
	loc      *time.Location
	dayStart time.Time
	monStart time.Time
	day      map[int64]*counter
	month    map[int64]*counter
	running  map[int64]int
	flights  map[string]inflight
	lastUsed map[int64]int64

	modelsMu sync.Mutex
	models   map[string]struct{}
}

func newState() *state {
	return &state{
		groups: map[int64]*Group{}, users: map[int64]*User{}, keys: map[int64]*Key{},
		byHash: map[string]int64{}, byScope: map[string]int64{}, allowed: map[int64][]string{},
		loc: time.Local, day: map[int64]*counter{}, month: map[int64]*counter{},
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
	s.groups, s.users, s.keys, s.byHash, s.byScope, s.allowed = g, u, k, byHash, byScope, allowed
	s.mu.Unlock()
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

// seedCounters replaces the quota counters with totals loaded from storage.
func (s *state) seedCounters(loc *time.Location, now time.Time, rows []hourlyRow) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.loc = loc
	s.dayStart, s.monStart = startOfDay(now, loc), startOfMonth(now, loc)
	s.day, s.month = map[int64]*counter{}, map[int64]*counter{}
	for _, r := range rows {
		t := time.Unix(r.Hour, 0)
		if t.Before(s.monStart) {
			continue
		}
		ok := r.Requests - r.Failed
		addTo(s.month, r.UserID, r.Total, ok)
		if !t.Before(s.dayStart) {
			addTo(s.day, r.UserID, r.Total, ok)
		}
	}
}

func addTo(m map[int64]*counter, userID, tokens, requests int64) {
	c := m[userID]
	if c == nil {
		c = &counter{}
		m[userID] = c
	}
	c.Tokens += tokens
	c.Requests += requests
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

func (s *state) addUsage(userID int64, at time.Time, tokens int64, success bool) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.rollLocked(time.Now())
	var reqs int64
	if success {
		reqs = 1
	}
	if !at.Before(s.monStart) {
		addTo(s.month, userID, tokens, reqs)
	}
	if !at.Before(s.dayStart) {
		addTo(s.day, userID, tokens, reqs)
	}
}

// QuotaView is the usage-versus-limit snapshot for one user.
type QuotaView struct {
	Day        counter `json:"day"`
	Month      counter `json:"month"`
	Running    int     `json:"running"`
	DayReset   int64   `json:"day_reset"`
	MonthReset int64   `json:"month_reset"`
}

func (s *state) quota(userID int64, now time.Time) QuotaView {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.rollLocked(now)
	v := QuotaView{Running: s.running[userID]}
	if c := s.day[userID]; c != nil {
		v.Day = *c
	}
	if c := s.month[userID]; c != nil {
		v.Month = *c
	}
	v.DayReset = s.dayStart.AddDate(0, 0, 1).Unix()
	v.MonthReset = s.monStart.AddDate(0, 1, 0).Unix()
	return v
}

// quotaExceeded returns a reason and reset time when any limit is used up.
// Token limits are checked before a request, so one request can overshoot;
// the next one is refused.
func quotaExceeded(u User, q QuotaView) (string, time.Time) {
	switch {
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

const staleFlight = 30 * time.Minute

// acquire admits requestID for userID unless the user is at max. Repeated
// calls for the same request are idempotent.
func (s *state) acquire(requestID string, userID int64, max int, now time.Time) bool {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if _, ok := s.flights[requestID]; ok {
		return true
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
		return false
	}
	s.flights[requestID] = inflight{userID: userID, at: now}
	s.running[userID]++
	return true
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
