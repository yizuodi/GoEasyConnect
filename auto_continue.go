package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultAutoContinueCount   = 3
	defaultAutoContinueMinutes = 20
	autoContinuePrompt         = "继续"
	maxCodexStateTailBytes     = 2 * 1024 * 1024
)

type AutoContinueManager struct {
	app      *App
	mu       sync.Mutex
	now      func() time.Time
	inspect  func(Session) (running, busy, known bool)
	send     func(Session) error
	stop     chan struct{}
	done     chan struct{}
	start    sync.Once
	stopOnce sync.Once
}

func newAutoContinueManager(app *App) *AutoContinueManager {
	manager := &AutoContinueManager{
		app: app, now: time.Now, stop: make(chan struct{}), done: make(chan struct{}),
	}
	manager.inspect = manager.inspectSession
	manager.send = manager.sendContinue
	return manager
}

func (m *AutoContinueManager) Start() {
	m.start.Do(func() {
		go func() {
			defer close(m.done)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					m.Check()
				case <-m.stop:
					return
				}
			}
		}()
	})
}

func (m *AutoContinueManager) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.done
}

func (m *AutoContinueManager) Check() {
	now := m.now()
	sessions, err := m.app.store.listSessions()
	if err != nil {
		m.app.logError("list auto-continue sessions", err)
		return
	}
	for _, session := range sessions {
		if !session.AutoContinueEnabled {
			continue
		}
		if err := m.checkSession(session, now); err != nil {
			m.app.logError("auto-continue session "+session.ID, err)
		}
	}
}

func (m *AutoContinueManager) checkSession(session Session, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session.Agent != "codex" || session.AutoContinueRemain <= 0 {
		return m.app.store.disableAutoContinue(session.ID)
	}
	minutes := session.AutoContinueMinutes
	if minutes <= 0 {
		minutes = defaultAutoContinueMinutes
	}
	next := now.Add(time.Duration(minutes) * time.Minute)
	if session.AutoContinueNextAt == "" {
		running, _, _ := m.inspect(session)
		if !running {
			return nil
		}
		return m.app.store.rescheduleAutoContinue(session.ID, next.UTC().Format(time.RFC3339))
	}
	due, err := time.Parse(time.RFC3339, session.AutoContinueNextAt)
	if err != nil {
		return m.app.store.rescheduleAutoContinue(session.ID, next.UTC().Format(time.RFC3339))
	}
	if now.Before(due) {
		return nil
	}
	running, busy, known := m.inspect(session)
	if !running {
		return m.app.store.rescheduleAutoContinue(session.ID, "")
	}
	if !known || busy {
		return m.app.store.rescheduleAutoContinue(session.ID, next.UTC().Format(time.RFC3339))
	}
	latest, err := m.app.store.getSession(session.ID)
	if err != nil {
		return err
	}
	if !latest.AutoContinueEnabled {
		return nil
	}
	running, busy, known = m.inspect(latest)
	if !running {
		return m.app.store.rescheduleAutoContinue(session.ID, "")
	}
	if !known || busy {
		return m.app.store.rescheduleAutoContinue(session.ID, next.UTC().Format(time.RFC3339))
	}
	if err := m.send(latest); err != nil {
		_ = m.app.store.rescheduleAutoContinue(session.ID, next.UTC().Format(time.RFC3339))
		return err
	}
	if err := m.app.store.recordAutoContinue(session.ID, next.UTC().Format(time.RFC3339)); err != nil {
		_ = m.app.store.disableAutoContinue(session.ID)
		return err
	}
	return nil
}

func (m *AutoContinueManager) Configure(session Session, enabled bool, total, minutes int) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !enabled {
		if err := m.app.store.disableAutoContinue(session.ID); err != nil {
			return Session{}, err
		}
		return m.app.store.getSession(session.ID)
	}
	if err := validateAutoContinue(total, minutes); err != nil {
		return Session{}, err
	}
	nextAt := ""
	if m.app.sessions.IsRunning(session.ID) || m.app.conversations.IsRunning(session.ID) {
		nextAt = autoContinueNext(m.now(), minutes)
	}
	if err := m.app.store.setAutoContinue(session.ID, true, total, minutes, nextAt); err != nil {
		return Session{}, err
	}
	return m.app.store.getSession(session.ID)
}

func (m *AutoContinueManager) SessionStarted(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.app.store.getSession(id)
	if err != nil || !session.AutoContinueEnabled {
		return
	}
	minutes := session.AutoContinueMinutes
	if minutes <= 0 {
		minutes = defaultAutoContinueMinutes
	}
	next := m.now().Add(time.Duration(minutes) * time.Minute).UTC().Format(time.RFC3339)
	if err := m.app.store.rescheduleAutoContinue(id, next); err != nil {
		m.app.logError("schedule auto-continue", err)
	}
}

func (m *AutoContinueManager) SessionStopped(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.app.store.rescheduleAutoContinue(id, ""); err != nil {
		m.app.logError("pause auto-continue", err)
	}
}

func (m *AutoContinueManager) inspectSession(session Session) (running, busy, known bool) {
	if session.RunMode == "conversation" {
		if !m.app.conversations.IsRunning(session.ID) {
			return false, false, true
		}
		return true, m.app.conversations.IsTurnRunning(session.ID), true
	}
	terminal := m.app.sessions.get(session.ID)
	if terminal == nil {
		return false, false, true
	}
	busy, known = terminal.CodexTaskRunning()
	return true, busy, known
}

func (m *AutoContinueManager) sendContinue(session Session) error {
	if session.RunMode == "conversation" {
		return m.sendConversationContinue(session)
	}
	return m.sendTerminalContinue(session)
}

func (m *AutoContinueManager) sendConversationContinue(session Session) error {
	if m.app.conversations.IsTurnRunning(session.ID) {
		return errors.New("Codex conversation became busy before auto-continue")
	}
	userID, assistantID := uuid.NewString(), uuid.NewString()
	if err := m.app.store.createConversationMessage(Message{ID: userID, SessionID: session.ID, Role: "user", Content: autoContinuePrompt, SourceID: userID}); err != nil {
		return err
	}
	if err := m.app.store.createConversationMessage(Message{ID: assistantID, SessionID: session.ID, Role: "assistant"}); err != nil {
		_ = m.app.store.deleteMessage(userID)
		return err
	}
	if _, err := m.app.conversations.StartTurn(session.ID, autoContinuePrompt, assistantID, userID); err != nil {
		_ = m.app.store.deleteMessage(userID)
		_ = m.app.store.deleteMessage(assistantID)
		return err
	}
	return nil
}

func (m *AutoContinueManager) sendTerminalContinue(session Session) error {
	terminal := m.app.sessions.get(session.ID)
	if terminal == nil {
		return errors.New("Codex terminal session is not running")
	}
	userID, assistantID := uuid.NewString(), uuid.NewString()
	if err := m.app.store.createMessage(Message{ID: userID, SessionID: session.ID, Role: "user", Content: autoContinuePrompt}); err != nil {
		return err
	}
	if err := m.app.store.createMessage(Message{ID: assistantID, SessionID: session.ID, Role: "assistant"}); err != nil {
		_ = m.app.store.deleteMessage(userID)
		return err
	}
	terminal.BeginMessage(assistantID)
	if err := terminal.SubmitMessage(autoContinuePrompt); err != nil {
		_ = m.app.store.deleteMessage(userID)
		_ = m.app.store.deleteMessage(assistantID)
		return err
	}
	terminal.broadcast(map[string]any{"type": "user_message", "id": userID, "session_id": session.ID, "role": "user", "content": autoContinuePrompt, "automatic": true})
	return nil
}

func (t *TerminalSession) CodexTaskRunning() (busy, known bool) {
	t.detectSessionID(false)
	t.mu.Lock()
	agent, sessionID, sessionDir := t.agent, t.agentSessionID, t.sessionDir
	t.mu.Unlock()
	if agent != "codex" || sessionID == "" {
		return false, false
	}
	path, err := findCodexSessionFile(sessionDir, sessionID)
	if err != nil {
		return false, false
	}
	return readCodexTaskState(path)
}

func findCodexSessionFile(root, sessionID string) (string, error) {
	var selected string
	var selectedTime time.Time
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") || !strings.Contains(entry.Name(), sessionID) {
			return nil
		}
		info, err := entry.Info()
		if err == nil && (selected == "" || info.ModTime().After(selectedTime)) {
			selected, selectedTime = path, info.ModTime()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if selected == "" {
		return "", os.ErrNotExist
	}
	return selected, nil
}

func readCodexTaskState(path string) (busy, known bool) {
	file, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > maxCodexStateTailBytes {
		_, _ = file.Seek(-maxCodexStateTailBytes, io.SeekEnd)
		reader := bufio.NewReader(file)
		_, _ = reader.ReadString('\n')
		return scanCodexTaskState(reader)
	}
	return scanCodexTaskState(file)
}

func scanCodexTaskState(reader io.Reader) (busy, known bool) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxCodexJSONLine)
	for scanner.Scan() {
		var event struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "event_msg" {
			continue
		}
		switch event.Payload.Type {
		case "task_started":
			busy, known = true, true
		case "task_complete", "turn_aborted":
			busy, known = false, true
		}
	}
	return busy, known
}

func autoContinueNext(now time.Time, minutes int) string {
	return now.Add(time.Duration(minutes) * time.Minute).UTC().Format(time.RFC3339)
}

func validateAutoContinue(total, minutes int) error {
	if total < 1 || total > 1000 {
		return fmt.Errorf("trigger count must be between 1 and 1000")
	}
	if minutes < 1 || minutes > 1440 {
		return fmt.Errorf("check interval must be between 1 and 1440 minutes")
	}
	return nil
}
