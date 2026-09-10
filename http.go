package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

//go:embed web/*
var embeddedWeb embed.FS

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", a.handlePublicConfig)
	mux.Handle("GET /api/app-config", a.requireAuth(http.HandlerFunc(a.handleAppConfig)))
	mux.Handle("GET /api/defaults", a.requireAuth(http.HandlerFunc(a.handleDefaults)))
	mux.Handle("GET /api/profiles", a.requireAuth(http.HandlerFunc(a.handleListProfiles)))
	mux.Handle("POST /api/profiles", a.requireAuth(http.HandlerFunc(a.handleCreateProfile)))
	mux.Handle("PUT /api/profiles/{id}", a.requireAuth(http.HandlerFunc(a.handleUpdateProfile)))
	mux.Handle("DELETE /api/profiles/{id}", a.requireAuth(http.HandlerFunc(a.handleDeleteProfile)))
	mux.Handle("GET /api/sessions", a.requireAuth(http.HandlerFunc(a.handleListSessions)))
	mux.Handle("POST /api/sessions", a.requireAuth(http.HandlerFunc(a.handleCreateSession)))
	mux.Handle("DELETE /api/sessions/{id}", a.requireAuth(http.HandlerFunc(a.handleDeleteSession)))
	mux.Handle("POST /api/sessions/{id}/start", a.requireAuth(http.HandlerFunc(a.handleStartSession)))
	mux.Handle("POST /api/sessions/{id}/stop", a.requireAuth(http.HandlerFunc(a.handleStopSession)))
	mux.Handle("PATCH /api/sessions/{id}/skip-permissions", a.requireAuth(http.HandlerFunc(a.handleSkipPermissions)))
	mux.Handle("PATCH /api/sessions/{id}/profile", a.requireAuth(http.HandlerFunc(a.handleSessionProfile)))
	mux.Handle("GET /api/sessions/{id}/messages", a.requireAuth(http.HandlerFunc(a.handleListMessages)))
	mux.Handle("POST /api/sessions/{id}/messages", a.requireAuth(http.HandlerFunc(a.handleCreateMessage)))
	mux.Handle("GET /api/sessions/{id}/output", a.requireAuth(http.HandlerFunc(a.handlePollOutput)))
	mux.Handle("POST /api/sessions/{id}/input", a.requireAuth(http.HandlerFunc(a.handleTerminalInput)))
	mux.Handle("POST /api/sessions/{id}/resize", a.requireAuth(http.HandlerFunc(a.handleTerminalResize)))
	mux.Handle("POST /api/ws-ticket", a.requireAuth(http.HandlerFunc(a.handleWebSocketTicket)))
	mux.HandleFunc("GET /ws", a.handleWebSocket)
	mux.HandleFunc("GET /", a.handleAsset)
	return securityHeaders(mux)
}

func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !constantTimeEqual(r.Header.Get("Authorization"), "Bearer "+a.cfg.Auth.Password) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func constantTimeEqual(actual, expected string) bool {
	actualHash := sha256.Sum256([]byte(actual))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(actualHash[:], expectedHash[:]) == 1
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'self'; form-action 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self' ws: wss:")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), fullscreen=(self)")
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleAsset(w http.ResponseWriter, r *http.Request) {
	asset := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	switch asset {
	case "", ".":
		asset = "index.html"
	case "mobile":
		asset = "mobile.html"
	}
	allowed := map[string]bool{
		"index.html": true, "mobile.html": true, "app.js": true, "style.css": true,
		"xterm.js": true, "xterm.css": true, "xterm-addon-fit.js": true,
	}
	if !allowed[asset] {
		http.NotFound(w, r)
		return
	}
	content, err := fs.ReadFile(embeddedWeb, "web/"+asset)
	if err != nil {
		a.writeInternalError(w, "read embedded asset", err)
		return
	}
	if extension := filepath.Ext(asset); extension != "" {
		w.Header().Set("Content-Type", mime.TypeByExtension(extension))
	}
	// Assets are compiled into the binary; revalidation keeps browser and
	// backend versions in lockstep after replacing the executable.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (a *App) handlePublicConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"branding": a.cfg.Branding})
}

func (a *App) handleAppConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"branding": a.cfg.Branding, "pollIntervalMs": a.cfg.Session.PollIntervalMS,
		"defaultWorkingDir": a.cfg.DefaultWorkingDir, "defaultAgent": a.cfg.Session.DefaultAgent,
		"agents": map[string]string{"claude": "Claude Code", "codex": "Codex"}, "fileBrowserUrl": a.cfg.FileBrowser.URL,
	})
}

func (a *App) handleDefaults(w http.ResponseWriter, _ *http.Request) {
	profileContent := a.cfg.Defaults.ProfileContent
	if profileContent == nil {
		profileContent = map[string]any{
			"hasCompletedOnboarding": true,
			"env": map[string]string{
				"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_BASE_URL": "", "ANTHROPIC_DEFAULT_OPUS_MODEL": "",
				"ANTHROPIC_DEFAULT_SONNET_MODEL": "", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "",
			},
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profileContent": profileContent, "codexProfileContent": a.cfg.Defaults.CodexProfileContent, "workingDir": a.cfg.DefaultWorkingDir})
}

func (a *App) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent != "" && !validAgent(agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Agent must be claude or codex"})
		return
	}
	profiles, err := a.store.listProfiles(agent)
	if err != nil {
		a.writeInternalError(w, "list profiles", err)
		return
	}
	writeJSON(w, http.StatusOK, profiles)
}

type createProfileRequest struct {
	Name        string `json:"name"`
	Agent       string `json:"agent"`
	Description string `json:"description"`
	Content     any    `json:"content"`
}

func (a *App) handleCreateProfile(w http.ResponseWriter, r *http.Request) {
	var request createProfileRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Name is required"})
		return
	}
	if request.Agent == "" {
		request.Agent = "claude"
	}
	if !validAgent(request.Agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Agent must be claude or codex"})
		return
	}
	content, err := normalizeProfileContent(request.Agent, request.Content)
	if err == nil && request.Agent == "codex" {
		err = a.validateCodexProfile(content)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Invalid %s content: %s", profileFormat(request.Agent), err.Error())})
		return
	}
	profile := Profile{ID: uuid.NewString(), Name: request.Name, Agent: request.Agent, Description: request.Description, Content: content}
	if err := a.store.createProfile(profile); err != nil {
		if isUniqueConstraint(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Profile name already exists"})
			return
		}
		a.writeInternalError(w, "create profile", err)
		return
	}
	if err := a.writeProfileFile(profile, content); err != nil {
		_ = a.store.deleteProfile(profile.ID)
		a.writeInternalError(w, "write profile file", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": profile.ID, "name": profile.Name, "agent": profile.Agent})
}

type updateProfileRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Content     *any    `json:"content"`
}

func (a *App) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := a.store.getProfile(r.PathValue("id"))
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get profile", err)
		return
	}
	var request updateProfileRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	name, description, content := profile.Name, profile.Description, profile.Content
	if request.Name != nil && strings.TrimSpace(*request.Name) != "" {
		name = strings.TrimSpace(*request.Name)
	}
	if request.Description != nil {
		description = *request.Description
	}
	if request.Content != nil {
		content, err = normalizeProfileContent(profile.Agent, *request.Content)
		if err == nil && profile.Agent == "codex" {
			err = a.validateCodexProfile(content)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Invalid %s content: %s", profileFormat(profile.Agent), err.Error())})
			return
		}
	}
	if err := a.store.updateProfile(profile.ID, name, description, content); err != nil {
		if isUniqueConstraint(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Profile name already exists"})
			return
		}
		a.writeInternalError(w, "update profile", err)
		return
	}
	if err := a.writeProfileFile(profile, content); err != nil {
		_ = a.store.updateProfile(profile.ID, profile.Name, profile.Description, profile.Content)
		a.writeInternalError(w, "write updated profile file", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := a.store.getProfile(r.PathValue("id"))
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get profile for deletion", err)
		return
	}
	if err := a.store.deleteProfile(profile.ID); err != nil {
		a.writeInternalError(w, "delete profile", err)
		return
	}
	if err := os.Remove(a.profileFilePath(profile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.logError("remove profile file", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024)
	if err := json.NewDecoder(r.Body).Decode(destination); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON request"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func parseBoundedInt(value string, fallback, minimum, maximum int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	if parsed < minimum {
		return minimum
	}
	if parsed > maximum {
		return maximum
	}
	return parsed
}

func profileFormat(agent string) string {
	if agent == "codex" {
		return "TOML"
	}
	return "JSON"
}
