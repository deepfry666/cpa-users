package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// CPA refuses plugins without author and repository metadata. Forks can
// override them: go build -ldflags "-X main.pluginRepo=... -X main.pluginAuthor=..."
var (
	pluginAuthor = "deepfry666"
	pluginRepo   = "https://github.com/deepfry666/cpa-users"
	pluginLogo   = "https://raw.githubusercontent.com/deepfry666/cpa-users/main/assets/logo.svg"
)

// App owns the plugin's configuration, storage and in-memory state.
type App struct {
	mu      sync.RWMutex
	cfg     Config
	store   *store
	initErr string
	state   *state

	stop    chan struct{}
	started bool
	dirty   chan struct{}
}

func newApp() *App {
	return &App{state: newState(), cfg: defaultConfig(), stop: make(chan struct{}), dirty: make(chan struct{}, 1)}
}

func (a *App) config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *App) db() *store {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.store
}

// ready reports whether storage is open and the plugin is enabled.
func (a *App) ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Enabled && a.store != nil
}

func (a *App) dispatch(method string, payload []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		return a.configure(payload)
	case methodPluginQuiesce:
		return okEnvelope(map[string]any{})
	case methodPluginShutdown:
		a.shutdown()
		return okEnvelope(map[string]any{})
	case methodFrontendAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: PluginID})
	case methodFrontendAuthAuthenticate:
		return a.authenticate(payload)
	case methodRequestInterceptBefore:
		return a.interceptBefore(payload)
	case methodRequestInterceptAfter:
		return okEnvelope(requestInterceptResponse{})
	case methodRequestComplete:
		return a.complete(payload)
	case methodResponseInterceptAfter:
		return a.interceptResponse(payload)
	case methodUsageHandle:
		return a.handleUsage(payload)
	case methodManagementRegister:
		return okEnvelope(a.managementRegistration())
	case methodManagementHandle:
		return a.handleManagement(payload)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func (a *App) configure(payload []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg, err := parseConfig(req.ConfigYAML)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	reopen := a.store == nil || a.cfg.DBPath != cfg.DBPath
	a.cfg = cfg
	if cfg.Enabled && reopen {
		if a.store != nil {
			_ = a.store.close()
			a.store = nil
		}
		st, errOpen := openStore(cfg.DBPath)
		if errOpen != nil {
			a.initErr = errOpen.Error()
			log.Printf("[%s] open store: %v", PluginID, errOpen)
		} else {
			a.store, a.initErr = st, ""
		}
	}
	st := a.store
	a.mu.Unlock()

	if st != nil {
		if err := a.reload(); err != nil {
			log.Printf("[%s] load data: %v", PluginID, err)
		}
		a.seedCounters()
		if raw := st.getMeta("models"); raw != "" {
			var ids []string
			if json.Unmarshal([]byte(raw), &ids) == nil {
				a.state.noteModels(ids...)
			}
		}
		a.startBackground()
	}
	return okEnvelope(a.registration())
}

func (a *App) registration() registration {
	cfg := a.config()
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			Name:             PluginID,
			Version:          PluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			Logo:             pluginLogo,
			ConfigFields:     configFields(),
		},
		Capabilities: capabilities{
			FrontendAuthProvider:             true,
			FrontendAuthProviderOmitBody:     true,
			RequestInterceptor:               true,
			RequestInterceptorOmitBodyBefore: true,
			RequestInterceptorOmitBodyAfter:  true,
			RequestLifecyclePlugin:           true,
			ResponseInterceptor:              cfg.filterModels(),
			UsagePlugin:                      true,
			ManagementAPI:                    true,
		},
	}
}

// reload rebuilds the in-memory index from SQLite.
func (a *App) reload() error {
	st := a.db()
	if st == nil {
		return fmt.Errorf("storage unavailable")
	}
	groups, users, keys, err := st.loadAll()
	if err != nil {
		return err
	}
	a.state.rebuild(groups, users, keys)
	return nil
}

func (a *App) seedCounters() {
	st := a.db()
	if st == nil {
		return
	}
	loc := a.config().location()
	now := time.Now()
	rows, err := st.hourly(startOfMonth(now, loc), now.Add(time.Hour), 0)
	if err != nil {
		log.Printf("[%s] seed counters: %v", PluginID, err)
		return
	}
	a.state.seedCounters(loc, now, rows)
}

func (a *App) markModelsDirty() {
	select {
	case a.dirty <- struct{}{}:
	default:
	}
}

// startBackground flushes key last-used times and discovered models, and
// prunes old request logs.
func (a *App) startBackground() {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	a.started = true
	stop := a.stop
	a.mu.Unlock()

	go func() {
		flush := time.NewTicker(15 * time.Second)
		prune := time.NewTicker(6 * time.Hour)
		defer flush.Stop()
		defer prune.Stop()
		a.prune()
		for {
			select {
			case <-stop:
				return
			case <-flush.C:
				a.flush()
			case <-a.dirty:
				a.saveModels()
			case <-prune.C:
				a.prune()
			}
		}
	}()
}

func (a *App) flush() {
	st := a.db()
	if st == nil {
		return
	}
	if err := st.touchKeys(a.state.drainTouches()); err != nil {
		log.Printf("[%s] flush key usage: %v", PluginID, err)
	}
}

func (a *App) saveModels() {
	st := a.db()
	if st == nil {
		return
	}
	raw, _ := json.Marshal(a.state.knownModels())
	if err := st.setMeta("models", string(raw)); err != nil {
		log.Printf("[%s] save models: %v", PluginID, err)
	}
}

func (a *App) prune() {
	st := a.db()
	if st == nil {
		return
	}
	days := a.config().LogRetentionDays
	if err := st.pruneLogs(time.Now().AddDate(0, 0, -days)); err != nil {
		log.Printf("[%s] prune logs: %v", PluginID, err)
	}
}

// shutdown stops background work and closes storage. A later
// plugin.register reopens everything.
func (a *App) shutdown() {
	a.mu.Lock()
	if a.started {
		close(a.stop)
		a.stop = make(chan struct{})
		a.started = false
	}
	a.mu.Unlock()
	a.flush()
	a.saveModels()
	a.mu.Lock()
	if a.store != nil {
		_ = a.store.close()
		a.store = nil
	}
	a.mu.Unlock()
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
