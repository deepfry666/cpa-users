package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Group is a reusable model allowlist.
type Group struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Models    []string `json:"models"`
	Note      string   `json:"note"`
	CreatedAt int64    `json:"created_at"`
}

// User owns keys, a model scope and optional limits. Zero limits mean unlimited.
type User struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Note            string   `json:"note"`
	GroupID         int64    `json:"group_id"`
	ExtraModels     []string `json:"extra_models"`
	Disabled        bool     `json:"disabled"`
	ExpiresAt       int64    `json:"expires_at"`
	DailyTokens     int64    `json:"daily_tokens"`
	MonthlyTokens   int64    `json:"monthly_tokens"`
	DailyRequests   int64    `json:"daily_requests"`
	MonthlyRequests int64    `json:"monthly_requests"`
	MaxConcurrency  int      `json:"max_concurrency"`
	CreatedAt       int64    `json:"created_at"`
}

// Key is an issued downstream credential. Only its SHA-256 is stored.
type Key struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Name       string `json:"name"`
	Hint       string `json:"hint"`
	Disabled   bool   `json:"disabled"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	hash       string
}

// usageRow is one completed upstream execution attributed to a user.
type usageRow struct {
	At                                time.Time
	UserID, KeyID                     int64
	Model, Provider, RequestID, Error string
	Stream, Failed                    bool
	Status                            int
	Input, Output, Reasoning, Cached  int64
	Total, LatencyMs, TTFTMs          int64
}

// hourlyRow is one aggregated hour bucket.
type hourlyRow struct {
	Hour                             int64
	UserID, KeyID                    int64
	Model                            string
	Requests, Failed                 int64
	Input, Output, Reasoning, Cached int64
	Total                            int64
}

// LogEntry is a per-request record shown in the console.
type LogEntry struct {
	ID        int64  `json:"id"`
	TS        int64  `json:"ts"`
	UserID    int64  `json:"user_id"`
	KeyID     int64  `json:"key_id"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Stream    bool   `json:"stream"`
	Failed    bool   `json:"failed"`
	Status    int    `json:"status"`
	Input     int64  `json:"input_tokens"`
	Output    int64  `json:"output_tokens"`
	Total     int64  `json:"total_tokens"`
	LatencyMs int64  `json:"latency_ms"`
	TTFTMs    int64  `json:"ttft_ms"`
	Error     string `json:"error,omitempty"`
}

type store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS groups (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	models TEXT NOT NULL DEFAULT '[]',
	note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	note TEXT NOT NULL DEFAULT '',
	group_id INTEGER NOT NULL DEFAULT 0,
	extra_models TEXT NOT NULL DEFAULT '[]',
	disabled INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL DEFAULT 0,
	daily_tokens INTEGER NOT NULL DEFAULT 0,
	monthly_tokens INTEGER NOT NULL DEFAULT 0,
	daily_requests INTEGER NOT NULL DEFAULT 0,
	monthly_requests INTEGER NOT NULL DEFAULT 0,
	max_concurrency INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS keys (
	id INTEGER PRIMARY KEY,
	user_id INTEGER NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	hash TEXT NOT NULL UNIQUE,
	hint TEXT NOT NULL DEFAULT '',
	disabled INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS usage_hourly (
	hour INTEGER NOT NULL,
	user_id INTEGER NOT NULL,
	key_id INTEGER NOT NULL,
	model TEXT NOT NULL,
	requests INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (hour, user_id, key_id, model)
);
CREATE TABLE IF NOT EXISTS request_log (
	id INTEGER PRIMARY KEY,
	ts INTEGER NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	user_id INTEGER NOT NULL,
	key_id INTEGER NOT NULL,
	model TEXT NOT NULL,
	provider TEXT NOT NULL DEFAULT '',
	stream INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ttft_ms INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS request_log_ts ON request_log (ts);
CREATE INDEX IF NOT EXISTS request_log_user_ts ON request_log (user_id, ts);
CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

func openStore(path string) (*store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	// One connection serialises writers and avoids SQLITE_BUSY between them.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &store{db: db}, nil
}

func (s *store) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func encodeList(list []string) string {
	clean := normalizePatterns(list)
	raw, _ := json.Marshal(clean)
	return string(raw)
}

func decodeList(raw string) []string {
	var list []string
	_ = json.Unmarshal([]byte(raw), &list)
	if list == nil {
		list = []string{}
	}
	return list
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *store) loadAll() ([]Group, []User, []Key, error) {
	var groups []Group
	rows, err := s.db.Query(`SELECT id, name, models, note, created_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var g Group
		var models string
		if err := rows.Scan(&g.ID, &g.Name, &models, &g.Note, &g.CreatedAt); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		g.Models = decodeList(models)
		groups = append(groups, g)
	}
	rows.Close()

	var users []User
	rows, err = s.db.Query(`SELECT id, name, note, group_id, extra_models, disabled, expires_at, daily_tokens,
		monthly_tokens, daily_requests, monthly_requests, max_concurrency, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var u User
		var extra string
		var disabled int
		if err := rows.Scan(&u.ID, &u.Name, &u.Note, &u.GroupID, &extra, &disabled, &u.ExpiresAt, &u.DailyTokens,
			&u.MonthlyTokens, &u.DailyRequests, &u.MonthlyRequests, &u.MaxConcurrency, &u.CreatedAt); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		u.ExtraModels = decodeList(extra)
		u.Disabled = disabled != 0
		users = append(users, u)
	}
	rows.Close()

	var keys []Key
	rows, err = s.db.Query(`SELECT id, user_id, name, hash, hint, disabled, created_at, last_used_at FROM keys ORDER BY id`)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var k Key
		var disabled int
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.hash, &k.Hint, &disabled, &k.CreatedAt, &k.LastUsedAt); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		k.Disabled = disabled != 0
		keys = append(keys, k)
	}
	rows.Close()
	return groups, users, keys, rows.Err()
}

var errDuplicateName = errors.New("名称已存在")

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *store) saveGroup(g Group) (int64, error) {
	if g.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO groups (name, models, note, created_at) VALUES (?, ?, ?, ?)`,
			g.Name, encodeList(g.Models), g.Note, time.Now().Unix())
		if isUniqueViolation(err) {
			return 0, errDuplicateName
		}
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.Exec(`UPDATE groups SET name = ?, models = ?, note = ? WHERE id = ?`,
		g.Name, encodeList(g.Models), g.Note, g.ID)
	if isUniqueViolation(err) {
		return 0, errDuplicateName
	}
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return g.ID, nil
}

func (s *store) deleteGroup(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET group_id = 0 WHERE group_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM groups WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) saveUser(u User) (int64, error) {
	args := []any{u.Name, u.Note, u.GroupID, encodeList(u.ExtraModels), boolInt(u.Disabled), u.ExpiresAt,
		u.DailyTokens, u.MonthlyTokens, u.DailyRequests, u.MonthlyRequests, u.MaxConcurrency}
	if u.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO users (name, note, group_id, extra_models, disabled, expires_at, daily_tokens,
			monthly_tokens, daily_requests, monthly_requests, max_concurrency, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(args, time.Now().Unix())...)
		if isUniqueViolation(err) {
			return 0, errDuplicateName
		}
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.Exec(`UPDATE users SET name = ?, note = ?, group_id = ?, extra_models = ?, disabled = ?, expires_at = ?,
		daily_tokens = ?, monthly_tokens = ?, daily_requests = ?, monthly_requests = ?, max_concurrency = ?
		WHERE id = ?`, append(args, u.ID)...)
	if isUniqueViolation(err) {
		return 0, errDuplicateName
	}
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return u.ID, nil
}

// deleteUser removes the user and their keys. Usage history is kept so past
// totals stay correct; the console labels it as a deleted user.
func (s *store) deleteUser(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM keys WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) createKey(userID int64, name, hash, hint string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO keys (user_id, name, hash, hint, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, name, hash, hint, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *store) updateKey(id int64, name string, disabled bool) error {
	res, err := s.db.Exec(`UPDATE keys SET name = ?, disabled = ? WHERE id = ?`, name, boolInt(disabled), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *store) deleteKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM keys WHERE id = ?`, id)
	return err
}

func (s *store) touchKeys(lastUsed map[int64]int64) error {
	if len(lastUsed) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, ts := range lastUsed {
		if _, err := tx.Exec(`UPDATE keys SET last_used_at = MAX(last_used_at, ?) WHERE id = ?`, ts, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) recordUsage(r usageRow) error {
	hour := r.At.UTC().Truncate(time.Hour).Unix()
	failed := boolInt(r.Failed)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO usage_hourly (hour, user_id, key_id, model, requests, failed, input_tokens,
		output_tokens, reasoning_tokens, cached_tokens, total_tokens) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (hour, user_id, key_id, model) DO UPDATE SET
			requests = requests + 1,
			failed = failed + excluded.failed,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
			cached_tokens = cached_tokens + excluded.cached_tokens,
			total_tokens = total_tokens + excluded.total_tokens`,
		hour, r.UserID, r.KeyID, r.Model, failed, r.Input, r.Output, r.Reasoning, r.Cached, r.Total); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO request_log (ts, request_id, user_id, key_id, model, provider, stream, failed, status,
		input_tokens, output_tokens, total_tokens, latency_ms, ttft_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.At.Unix(), r.RequestID, r.UserID, r.KeyID, r.Model, r.Provider, boolInt(r.Stream), failed, r.Status,
		r.Input, r.Output, r.Total, r.LatencyMs, r.TTFTMs, r.Error); err != nil {
		return err
	}
	return tx.Commit()
}

// hourly returns hour buckets in [from, to).
func (s *store) hourly(from, to time.Time, userID int64) ([]hourlyRow, error) {
	query := `SELECT hour, user_id, key_id, model, requests, failed, input_tokens, output_tokens, reasoning_tokens,
		cached_tokens, total_tokens FROM usage_hourly WHERE hour >= ? AND hour < ?`
	args := []any{from.UTC().Truncate(time.Hour).Unix(), to.Unix()}
	if userID != 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.Query(query+` ORDER BY hour`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hourlyRow
	for rows.Next() {
		var r hourlyRow
		if err := rows.Scan(&r.Hour, &r.UserID, &r.KeyID, &r.Model, &r.Requests, &r.Failed, &r.Input, &r.Output,
			&r.Reasoning, &r.Cached, &r.Total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) logs(limit int, userID int64, failedOnly bool) ([]LogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, ts, user_id, key_id, model, provider, stream, failed, status, input_tokens, output_tokens,
		total_tokens, latency_ms, ttft_ms, error FROM request_log WHERE 1 = 1`
	var args []any
	if userID != 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if failedOnly {
		query += ` AND failed = 1`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		var stream, failed int
		if err := rows.Scan(&e.ID, &e.TS, &e.UserID, &e.KeyID, &e.Model, &e.Provider, &stream, &failed, &e.Status,
			&e.Input, &e.Output, &e.Total, &e.LatencyMs, &e.TTFTMs, &e.Error); err != nil {
			return nil, err
		}
		e.Stream, e.Failed = stream != 0, failed != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *store) pruneLogs(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM request_log WHERE ts < ?`, before.Unix())
	return err
}

func (s *store) getMeta(k string) string {
	var v string
	_ = s.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	return v
}

func (s *store) setMeta(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}
