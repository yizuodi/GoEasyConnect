package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type App struct {
	cfg        *Config
	store      *Store
	logger     *log.Logger
	sessions   *SessionManager
	server     *http.Server
	wsMu       sync.Mutex
	wsClients  map[*wsClient]struct{}
	wsTicketMu sync.Mutex
	wsTickets  map[string]time.Time
}

func newApp(cfg *Config, logger *log.Logger) (*App, error) {
	if err := ensureAgentDirectory(cfg.ClaudeSettingsDir, cfg.Claude.RunAsUser); err != nil {
		return nil, fmt.Errorf("prepare Claude settings directory: %w", err)
	}
	if err := ensureAgentDirectory(cfg.CodexHome, cfg.Codex.RunAsUser); err != nil {
		return nil, fmt.Errorf("prepare Codex home: %w", err)
	}
	store, err := openStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	app := &App{
		cfg: cfg, store: store, logger: logger,
		wsClients: make(map[*wsClient]struct{}),
		wsTickets: make(map[string]time.Time),
	}
	app.sessions = newSessionManager(app)
	profiles, err := store.listProfiles("")
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	for _, profile := range profiles {
		if err := app.writeProfileFile(profile, profile.Content); err != nil {
			logger.Printf("sync %s profile %s: %v", profile.Agent, profile.ID, err)
		}
	}
	return app, nil
}

func (a *App) close(ctx context.Context) error {
	if a.server != nil {
		if err := a.server.Shutdown(ctx); err != nil {
			a.logError("shutdown HTTP server", err)
		}
	}
	a.closeWebSockets()
	a.sessions.stopAll()
	return a.store.Close()
}

func (a *App) logError(operation string, err error) {
	if err != nil {
		a.logger.Printf("%s: %v", operation, err)
	}
}

func (a *App) writeInternalError(w http.ResponseWriter, operation string, err error) {
	a.logError(operation, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal server error"})
}

func normalizeAgent(agent string) string {
	if agent == "codex" {
		return "codex"
	}
	return "claude"
}

func validAgent(agent string) bool {
	return agent == "claude" || agent == "codex"
}

func agentLabel(agent string) string {
	if agent == "codex" {
		return "Codex"
	}
	return "Claude Code"
}

type byteRing struct {
	max  int
	data []byte
}

func (b *byteRing) Append(data []byte) {
	if b.max <= 0 || len(data) == 0 {
		return
	}
	if len(data) >= b.max {
		b.data = append(b.data[:0], data[len(data)-b.max:]...)
		return
	}
	overflow := len(b.data) + len(data) - b.max
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, data...)
}

func (b *byteRing) Reset() {
	b.data = b.data[:0]
}

func (b *byteRing) Bytes() []byte {
	return append([]byte(nil), b.data...)
}

type pollChunk struct {
	seq  int64
	data []byte
}

type pollBuffer struct {
	maxBytes int
	maxItems int
	bytes    int
	seq      int64
	chunks   []pollChunk
}

func (p *pollBuffer) Append(data []byte) {
	p.seq++
	if p.maxBytes > 0 && len(data) > p.maxBytes {
		data = data[len(data)-p.maxBytes:]
	}
	copyData := append([]byte(nil), data...)
	p.chunks = append(p.chunks, pollChunk{seq: p.seq, data: copyData})
	p.bytes += len(copyData)
	for len(p.chunks) > 0 && ((p.maxBytes > 0 && p.bytes > p.maxBytes) || (p.maxItems > 0 && len(p.chunks) > p.maxItems)) {
		p.bytes -= len(p.chunks[0].data)
		p.chunks[0].data = nil
		p.chunks = p.chunks[1:]
	}
}

func (p *pollBuffer) Since(after int64) ([]byte, int64) {
	if after >= p.seq {
		return nil, p.seq
	}
	result := make([]byte, 0)
	for _, chunk := range p.chunks {
		if chunk.seq > after {
			result = append(result, chunk.data...)
		}
	}
	return result, p.seq
}

type onceError struct {
	once sync.Once
	err  error
}

func (o *onceError) Do(fn func() error) error {
	o.once.Do(func() { o.err = fn() })
	return o.err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
