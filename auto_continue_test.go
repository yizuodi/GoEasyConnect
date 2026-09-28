package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadCodexTaskState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}\n")
	if busy, known := readCodexTaskState(path); !known || !busy {
		t.Fatalf("started state busy=%v known=%v", busy, known)
	}
	write("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\"}}\n")
	if busy, known := readCodexTaskState(path); !known || busy {
		t.Fatalf("completed state busy=%v known=%v", busy, known)
	}
	write("{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\"}}\n")
	if busy, known := readCodexTaskState(path); known || busy {
		t.Fatalf("unknown state busy=%v known=%v", busy, known)
	}
}

func TestAutoContinueOnlyConsumesSuccessfulIdleChecks(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	app.autoContinue.Stop()
	session := Session{ID: "auto-session", Name: "auto", Agent: "codex", WorkingDir: cfg.DefaultWorkingDir}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	if err := app.store.setAutoContinue(session.ID, true, 2, 20, now.Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	manager := newAutoContinueManager(app)
	manager.now = func() time.Time { return now }
	busy := true
	sends := 0
	manager.inspect = func(Session) (bool, bool, bool) { return true, busy, true }
	manager.send = func(Session) error { sends++; return nil }

	manager.Check()
	stored, err := app.store.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 0 || stored.AutoContinueRemain != 2 || !stored.AutoContinueEnabled {
		t.Fatalf("busy check sends=%d session=%#v", sends, stored)
	}

	busy = false
	if err := app.store.rescheduleAutoContinue(session.ID, now.Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	manager.Check()
	stored, err = app.store.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 1 || stored.AutoContinueRemain != 1 || !stored.AutoContinueEnabled {
		t.Fatalf("first idle check sends=%d session=%#v", sends, stored)
	}

	if err := app.store.rescheduleAutoContinue(session.ID, now.Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	manager.Check()
	stored, err = app.store.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 2 || stored.AutoContinueRemain != 0 || stored.AutoContinueEnabled || stored.AutoContinueNextAt != "" {
		t.Fatalf("final idle check sends=%d session=%#v", sends, stored)
	}
}

func TestAutoContinueUnknownOrStoppedDoesNotSend(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	app.autoContinue.Stop()
	session := Session{ID: "paused-auto", Name: "paused", Agent: "codex", WorkingDir: cfg.DefaultWorkingDir}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	if err := app.store.setAutoContinue(session.ID, true, 3, 20, now.Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	manager := newAutoContinueManager(app)
	manager.now = func() time.Time { return now }
	sends := 0
	manager.send = func(Session) error { sends++; return nil }
	manager.inspect = func(Session) (bool, bool, bool) { return true, false, false }
	manager.Check()
	manager.inspect = func(Session) (bool, bool, bool) { return false, false, true }
	if err := app.store.rescheduleAutoContinue(session.ID, now.Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	manager.Check()
	stored, err := app.store.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 0 || stored.AutoContinueRemain != 3 || stored.AutoContinueNextAt != "" {
		t.Fatalf("paused checks sends=%d session=%#v", sends, stored)
	}
}

func TestAutoContinuePausesAndResumesWithSession(t *testing.T) {
	cfg := testConfig(t)
	app := testApp(t, cfg)
	app.autoContinue.Stop()
	session := Session{ID: "auto-lifecycle", Name: "lifecycle", Agent: "codex", WorkingDir: cfg.DefaultWorkingDir}
	if err := app.store.createSession(session); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	if err := app.store.setAutoContinue(session.ID, true, 3, 20, ""); err != nil {
		t.Fatal(err)
	}

	manager := newAutoContinueManager(app)
	manager.now = func() time.Time { return now }
	manager.inspect = func(Session) (bool, bool, bool) { return false, false, true }
	manager.Check()
	stored, err := app.store.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AutoContinueNextAt != "" || !stored.AutoContinueEnabled || stored.AutoContinueRemain != 3 {
		t.Fatalf("stopped session should remain paused: %#v", stored)
	}

	manager.SessionStarted(session.ID)
	stored, err = app.store.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := now.Add(20 * time.Minute).Format(time.RFC3339)
	if stored.AutoContinueNextAt != wantNext {
		t.Fatalf("started next_at=%q want %q", stored.AutoContinueNextAt, wantNext)
	}

	manager.SessionStopped(session.ID)
	stored, err = app.store.getSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AutoContinueNextAt != "" || !stored.AutoContinueEnabled || stored.AutoContinueRemain != 3 {
		t.Fatalf("stopped session did not pause without resetting: %#v", stored)
	}
}
