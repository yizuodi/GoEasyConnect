package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

func (a *App) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	sessions, err := a.store.listSessions()
	if err != nil {
		a.writeInternalError(w, "list sessions", err)
		return
	}
	for index := range sessions {
		sessions[index].IsRunning = a.sessions.IsRunning(sessions[index].ID)
		if sessions[index].IsRunning {
			sessions[index].Status = "running"
		} else {
			sessions[index].Status = "stopped"
		}
	}
	writeJSON(w, http.StatusOK, sessions)
}

type createSessionRequest struct {
	Name       string  `json:"name"`
	Agent      string  `json:"agent"`
	ProfileID  *string `json:"profile_id"`
	WorkingDir string  `json:"working_dir"`
}

func (a *App) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var request createSessionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Name is required"})
		return
	}
	if request.Agent == "" {
		request.Agent = a.cfg.Session.DefaultAgent
	}
	if !validAgent(request.Agent) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Agent must be claude or codex"})
		return
	}
	var profileName *string
	if request.ProfileID != nil && *request.ProfileID != "" {
		profile, err := a.store.getProfile(*request.ProfileID)
		if isNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
			return
		}
		if err != nil {
			a.writeInternalError(w, "get selected profile", err)
			return
		}
		if profile.Agent != request.Agent {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Profile belongs to %s, not %s", profile.Agent, request.Agent)})
			return
		}
		profileName = &profile.Name
	} else {
		request.ProfileID = nil
	}
	workDir, err := filepath.Abs(firstNonEmpty(request.WorkingDir, a.cfg.DefaultWorkingDir))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid working directory"})
		return
	}
	runAsUser := a.cfg.Claude.RunAsUser
	if request.Agent == "codex" {
		runAsUser = a.cfg.Codex.RunAsUser
	}
	if err := ensureDirectory(workDir, 0o755, runAsUser); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Failed to create working directory: %s (%s)", workDir, err.Error())})
		return
	}
	session := Session{ID: uuid.NewString(), Name: request.Name, Agent: request.Agent, ProfileID: request.ProfileID, WorkingDir: workDir, ProfileName: profileName}
	if err := a.store.createSession(session); err != nil {
		a.writeInternalError(w, "create session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": session.ID, "name": session.Name, "agent": session.Agent, "profile_name": session.ProfileName})
}

func (a *App) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.store.getSession(id); isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	} else if err != nil {
		a.writeInternalError(w, "get session for deletion", err)
		return
	}
	a.sessions.Stop(id)
	if err := a.store.deleteSession(id); err != nil {
		a.writeInternalError(w, "delete session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleStartSession(w http.ResponseWriter, r *http.Request) {
	session, err := a.store.getSession(r.PathValue("id"))
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get session for start", err)
		return
	}
	if a.sessions.IsRunning(session.ID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Session already running"})
		return
	}
	if err := a.sessions.Start(session); err != nil {
		a.logError("start "+session.Agent+" session", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("Failed to start %s: %s", agentLabel(session.Agent), err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "running"})
}

func (a *App) handleStopSession(w http.ResponseWriter, r *http.Request) {
	session, err := a.store.getSession(r.PathValue("id"))
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get session for stop", err)
		return
	}
	a.sessions.Stop(session.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "stopped"})
}

func (a *App) handleSkipPermissions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.store.getSession(id); isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	} else if err != nil {
		a.writeInternalError(w, "get session for permissions", err)
		return
	}
	if a.sessions.IsRunning(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Cannot change while running"})
		return
	}
	var request struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if err := a.store.setSkipPermissions(id, request.Enabled); err != nil {
		a.writeInternalError(w, "set skip permissions", err)
		return
	}
	value := 0
	if request.Enabled {
		value = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skip_permissions": value})
}

func (a *App) handleSessionProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, err := a.store.getSession(id)
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get session for profile change", err)
		return
	}
	if a.sessions.IsRunning(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Cannot change profile while running"})
		return
	}
	var request struct {
		ProfileID *string `json:"profile_id"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.ProfileID != nil && *request.ProfileID != "" {
		profile, err := a.store.getProfile(*request.ProfileID)
		if isNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
			return
		}
		if err != nil {
			a.writeInternalError(w, "get profile for session", err)
			return
		}
		if profile.Agent != session.Agent {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Profile belongs to %s, not %s", profile.Agent, session.Agent)})
			return
		}
	} else {
		request.ProfileID = nil
	}
	if err := a.store.setSessionProfile(id, request.ProfileID); err != nil {
		a.writeInternalError(w, "set session profile", err)
		return
	}
	updated, err := a.store.getSession(id)
	if err != nil {
		a.writeInternalError(w, "reload updated session", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *App) handleListMessages(w http.ResponseWriter, r *http.Request) {
	limit := parseBoundedInt(r.URL.Query().Get("limit"), 200, 1, 1000)
	offset := parseBoundedInt(r.URL.Query().Get("offset"), 0, 0, 1<<30)
	messages, err := a.store.listMessages(r.PathValue("id"), limit, offset)
	if err != nil {
		a.writeInternalError(w, "list messages", err)
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

func (a *App) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Content is required"})
		return
	}
	session, err := a.store.getSession(r.PathValue("id"))
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get session for message", err)
		return
	}
	terminal := a.sessions.get(session.ID)
	if terminal == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Session not running"})
		return
	}
	messageID := uuid.NewString()
	if err := a.store.createMessage(Message{ID: messageID, SessionID: session.ID, Role: "user", Content: request.Content}); err != nil {
		a.writeInternalError(w, "save user message", err)
		return
	}
	assistantID := uuid.NewString()
	if err := a.store.createMessage(Message{ID: assistantID, SessionID: session.ID, Role: "assistant", Content: ""}); err != nil {
		a.writeInternalError(w, "create assistant message", err)
		return
	}
	terminal.BeginMessage(assistantID)
	if err := terminal.SubmitMessage(request.Content); err != nil {
		a.writeInternalError(w, "write terminal message", err)
		return
	}
	terminal.broadcast(map[string]any{"type": "user_message", "id": messageID, "session_id": session.ID, "role": "user", "content": request.Content})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "assistantMsgId": assistantID})
}

func (a *App) handlePollOutput(w http.ResponseWriter, r *http.Request) {
	terminal := a.sessions.get(r.PathValue("id"))
	if terminal == nil {
		writeJSON(w, http.StatusOK, map[string]any{"output": "", "seq": 0, "running": false})
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
	output, seq := terminal.Poll(after)
	writeJSON(w, http.StatusOK, map[string]any{"output": string(output), "seq": seq, "running": true})
}

func (a *App) handleTerminalInput(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Data string `json:"data"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Data == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Data is required"})
		return
	}
	terminal := a.sessions.get(r.PathValue("id"))
	if terminal == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Session not running"})
		return
	}
	if _, err := terminal.Write([]byte(request.Data)); err != nil {
		a.writeInternalError(w, "write terminal input", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleTerminalResize(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if terminal := a.sessions.get(r.PathValue("id")); terminal != nil {
		if err := terminal.Resize(request.Cols, request.Rows); err != nil {
			a.logError("resize terminal", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (t *TerminalSession) broadcast(value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	t.mu.Lock()
	clients := make([]*wsClient, 0, len(t.clients))
	for client := range t.clients {
		clients = append(clients, client)
	}
	t.mu.Unlock()
	for _, client := range clients {
		client.enqueue(payload)
	}
}
