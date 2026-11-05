package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var assets embed.FS

type Config struct {
	Addr, DataDir, AdminPassword, MasterKey, Version, VersionCode string
	Concurrency                                                   int
	Timeout                                                       time.Duration
	SecureCookie                                                  bool
}
type cachedModels struct {
	Models []Model
	At     time.Time
}
type Session struct {
	Messages []M
	At       time.Time
}
type App struct {
	cfg           Config
	store         *Store
	client        *http.Client
	mu            sync.Mutex
	refreshMu     sync.Mutex
	busy          map[string]int
	models        map[string]cachedModels
	sessions      map[string]Session
	logins        map[string]*LoginAttempt
	adminSessions map[string]time.Time
	loginGate     chan struct{}
	requests      chan struct{}
	root          context.Context
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func newApp(cfg Config) (*App, error) {
	if len(cfg.AdminPassword) < 16 {
		return nil, errors.New("RELAY_ADMIN_PASSWORD must contain at least 16 characters")
	}
	s, e := openStore(cfg.DataDir, cfg.MasterKey)
	if e != nil {
		return nil, e
	}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 45 * time.Second, MaxIdleConns: 32, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 16, IdleConnTimeout: 90 * time.Second}
	return &App{cfg: cfg, store: s, client: &http.Client{Transport: tr, Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, busy: map[string]int{}, models: map[string]cachedModels{}, sessions: map[string]Session{}, logins: map[string]*LoginAttempt{}, adminSessions: map[string]time.Time{}, loginGate: make(chan struct{}, 2), requests: make(chan struct{}, 64), root: context.Background()}, nil
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, M{"error": M{"type": "relay_error", "code": code, "message": code}})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	d := json.NewDecoder(r.Body)
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("extra JSON")
	}
	return nil
}
func (a *App) handler() http.Handler { return http.HandlerFunc(a.serve) }
func (a *App) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/health" && r.Method == "GET" {
		writeJSON(w, 200, M{"status": "ok"})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		select {
		case a.requests <- struct{}{}:
			defer func() { <-a.requests }()
		default:
			fail(w, 503, "server_busy")
			return
		}
		a.admin(w, r)
		return
	}
	if r.URL.Path == "/" && r.Method == "GET" {
		b, _ := assets.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		_, _ = w.Write(b)
		return
	}
	if r.URL.Path == "/app.js" || r.URL.Path == "/style.css" {
		b, e := assets.ReadFile("web" + r.URL.Path)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
		} else {
			w.Header().Set("Content-Type", "text/css")
		}
		_, _ = w.Write(b)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	select {
	case a.requests <- struct{}{}:
		defer func() { <-a.requests }()
	default:
		fail(w, 503, "server_busy")
		return
	}
	// Browser access is same-origin. No permissive CORS for a credential-bearing proxy.
	if r.Method == "OPTIONS" {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		w.WriteHeader(204)
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key == "" {
		key = r.Header.Get("X-API-Key")
	}
	if !a.store.validKey(key) {
		fail(w, 401, "invalid_api_key")
		return
	}
	switch {
	case r.URL.Path == "/v1/status" && r.Method == "GET":
		a.status(w)
	case r.URL.Path == "/v1/models" && r.Method == "GET":
		acc, ok := a.store.active()
		if !ok {
			fail(w, 503, "no_enabled_account")
			return
		}
		models, e := a.getModels(r.Context(), acc, false)
		if e != nil {
			fail(w, 502, "model_catalog_unavailable")
			return
		}
		out := []M{}
		for _, m := range models {
			st := a.store.setting(m.ID)
			if !st.Disabled {
				id := m.ID
				if st.Alias != "" {
					id = st.Alias
				}
				out = append(out, M{"id": id, "object": "model", "created": 0, "owned_by": "trae", "capabilities": m})
			}
		}
		writeJSON(w, 200, M{"object": "list", "data": out})
	case r.URL.Path == "/v1/chat/completions" && r.Method == "POST":
		a.chat(w, r, false)
	case r.URL.Path == "/v1/responses" && r.Method == "POST":
		a.chat(w, r, true)
	default:
		http.NotFound(w, r)
	}
}
func (a *App) status(w http.ResponseWriter) {
	a.mu.Lock()
	n := 0
	for _, v := range a.busy {
		n += v
	}
	a.mu.Unlock()
	_, ok := a.store.active()
	writeJSON(w, 200, M{"status": "ok", "active_account": ok, "in_flight": n, "max_concurrent_per_account": a.cfg.Concurrency})
}
func (a *App) acquire(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, ok := a.store.active()
	if !ok || current.ID != id {
		return false
	}
	if a.busy[id] >= a.cfg.Concurrency {
		return false
	}
	a.busy[id]++
	return true
}
func (a *App) release(id string) { a.mu.Lock(); defer a.mu.Unlock(); a.busy[id]-- }
func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		fmt.Println(base64Key())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		r, e := c.Get("http://127.0.0.1:" + env("PORT", "8317") + "/health")
		if e != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	n, _ := strconv.Atoi(env("RELAY_MAX_CONCURRENT", "2"))
	if n < 1 || n > 8 {
		log.Fatal("RELAY_MAX_CONCURRENT must be 1..8")
	}
	cfg := Config{Addr: env("RELAY_LISTEN", "0.0.0.0:"+env("PORT", "8317")), DataDir: env("RELAY_DATA_DIR", "data"), AdminPassword: os.Getenv("RELAY_ADMIN_PASSWORD"), MasterKey: os.Getenv("RELAY_MASTER_KEY"), Version: env("TRAE_IDE_VERSION", "3.4.0"), VersionCode: env("TRAE_IDE_VERSION_CODE", "20260919"), Concurrency: n, Timeout: 180 * time.Second}
	cfg.SecureCookie = env("RELAY_SECURE_COOKIE", "false") == "true"
	a, e := newApp(cfg)
	if e != nil {
		log.Fatal(e)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.root = ctx
	srv := &http.Server{Addr: cfg.Addr, Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	go a.catalogWorker(ctx)
	log.Printf("Trae Relay Web listening on %s", cfg.Addr)
	if e = srv.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
		log.Fatal(e)
	}
}
