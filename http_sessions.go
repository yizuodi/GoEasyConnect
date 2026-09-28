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
		sessions[index].TerminalRunning = a.sessions.IsRunning(sessions[index].ID)
		sessions[index].ConversationRunning = a.conversations.IsRunning(sessions[index].ID)
		sessions[index].TurnRunning = a.conversations.IsTurnRunning(sessions[index].ID)
		sessions[index].IsRunning = sessions[index].TerminalRunning || sessions[index].ConversationRunning
		if sessions[index].TerminalRunning {
			sessions[index].RunningMode = "terminal"
		}
		if sessions[index].ConversationRunning {
			sessions[index].RunningMode = "conversation"
		}
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
	RunMode    string  `json:"run_mode"`
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
	if request.RunMode == "" {
		request.RunMode = "terminal"
	}
	if request.RunMode != "terminal" && request.RunMode != "conversation" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Mode must be terminal or conversation"})
		return
	}
	if request.RunMode == "conversation" && request.Agent != "codex" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Conversation mode currently supports Codex only"})
		return
	}
	if request.RunMode == "conversation" && !a.cfg.Experimental.ConversationMode {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Conversation mode is disabled"})
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
	session := Session{ID: uuid.NewString(), Name: request.Name, Agent: request.Agent, ProfileID: request.ProfileID, WorkingDir: workDir, ProfileName: profileName, RunMode: request.RunMode}
	if err := a.store.createSession(session); err != nil {
		a.writeInternalError(w, "create session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": session.ID, "name": session.Name, "agent": session.Agent, "profile_name": session.ProfileName, "run_mode": session.RunMode})
}

type updateSessionRequest struct {
	Name string `json:"name"`
}

func (a *App) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, err := a.store.getSession(id)
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get session for update", err)
		return
	}
	if a.sessions.IsRunning(id) || a.conversations.IsRunning(id) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Stop the session before editing it"})
		return
	}
	var request updateSessionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Name is required"})
		return
	}
	if err := a.store.updateSession(id, request.Name); err != nil {
		if isNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
			return
		}
		a.writeInternalError(w, "update session", err)
		return
	}
	session.Name = request.Name
	writeJSON(w, http.StatusOK, session)
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
	a.conversations.StopSession(id)
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
	if a.sessions.IsRunning(session.ID) || a.conversations.IsRunning(session.ID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Session already running"})
		return
	}
	var startErr error
	if session.RunMode == "conversation" {
		startErr = a.conversations.StartSession(session)
	} else {
		startErr = a.sessions.Start(session)
	}
	if startErr != nil {
		a.logError("start "+session.Agent+" session", startErr)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("Failed to start %s: %s", agentLabel(session.Agent), startErr.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "running", "running_mode": session.RunMode})
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
	a.conversations.StopSession(session.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "stopped"})
}

func (a *App) handleSessionMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, err := a.store.getSession(id)
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get session for mode change", err)
		return
	}
	if a.sessions.IsRunning(id) || a.conversations.IsRunning(id) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Stop the session before changing mode"})
		return
	}
	var request struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Mode != "terminal" && request.Mode != "conversation" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Mode must be terminal or conversation"})
		return
	}
	if request.Mode == "conversation" {
		if !a.cfg.Experimental.ConversationMode {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Conversation mode is disabled"})
			return
		}
		if session.Agent != "codex" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Conversation mode currently supports Codex only"})
			return
		}
	}
	if err := a.store.setSessionRunMode(id, request.Mode); err != nil {
		a.writeInternalError(w, "set session mode", err)
		return
	}
	session.RunMode = request.Mode
	writeJSON(w, http.StatusOK, session)
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
	if a.sessions.IsRunning(id) || a.conversations.IsRunning(id) {
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
	if a.sessions.IsRunning(id) || a.conversations.IsRunning(id) {
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

func (a *App) handleListConversationMessages(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.Experimental.ConversationMode {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Conversation mode is disabled"})
		return
	}
	if _, err := a.store.getSession(r.PathValue("id")); isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	} else if err != nil {
		a.writeInternalError(w, "get conversation session", err)
		return
	}
	limit := parseBoundedInt(r.URL.Query().Get("limit"), 500, 1, 1000)
	offset := parseBoundedInt(r.URL.Query().Get("offset"), 0, 0, 1<<30)
	messages, err := a.store.listMessagesByChannel(r.PathValue("id"), "conversation", limit, offset)
	if err != nil {
		a.writeInternalError(w, "list conversation messages", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages, "running": a.conversations.IsTurnRunning(r.PathValue("id")), "session_running": a.conversations.IsRunning(r.PathValue("id"))})
}

func (a *App) handleCreateConversationMessage(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.Experimental.ConversationMode {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Conversation mode is disabled"})
		return
	}
	var request struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Content = strings.TrimSpace(request.Content)
	if request.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Content is required"})
		return
	}
	if len(request.Content) > 256*1024 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Message is too large"})
		return
	}
	session, err := a.store.getSession(r.PathValue("id"))
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	}
	if err != nil {
		a.writeInternalError(w, "get conversation session", err)
		return
	}
	if session.Agent != "codex" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Conversation mode currently supports Codex only"})
		return
	}
	if session.RunMode != "conversation" || !a.conversations.IsRunning(session.ID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Start the session in conversation mode before sending a message"})
		return
	}
	if a.conversations.IsTurnRunning(session.ID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "A conversation turn is already running"})
		return
	}
	userID, assistantID := uuid.NewString(), uuid.NewString()
	if err := a.store.createConversationMessage(Message{ID: userID, SessionID: session.ID, Role: "user", Content: request.Content, SourceID: userID}); err != nil {
		a.writeInternalError(w, "save conversation message", err)
		return
	}
	if err := a.store.createConversationMessage(Message{ID: assistantID, SessionID: session.ID, Role: "assistant", Content: ""}); err != nil {
		_ = a.store.deleteMessage(userID)
		a.writeInternalError(w, "create conversation response", err)
		return
	}
	turnID, err := a.conversations.StartTurn(session.ID, request.Content, assistantID, userID)
	if err != nil {
		_ = a.store.deleteMessage(userID)
		_ = a.store.deleteMessage(assistantID)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "turn_id": turnID, "user_message_id": userID, "assistant_message_id": assistantID,
	})
}

func (a *App) handleListConversationEvents(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.Experimental.ConversationMode {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Conversation mode is disabled"})
		return
	}
	if _, err := a.store.getSession(r.PathValue("id")); isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		return
	} else if err != nil {
		a.writeInternalError(w, "get conversation session", err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := parseBoundedInt(r.URL.Query().Get("limit"), 200, 1, 1000)
	events, err := a.store.listConversationEvents(r.PathValue("id"), after, limit)
	if err != nil {
		a.writeInternalError(w, "list conversation events", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "running": a.conversations.IsTurnRunning(r.PathValue("id")), "session_running": a.conversations.IsRunning(r.PathValue("id"))})
}

func (a *App) handleStopConversation(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.Experimental.ConversationMode {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Conversation mode is disabled"})
		return
	}
	interrupted := a.conversations.InterruptTurn(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "interrupted": interrupted})
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
