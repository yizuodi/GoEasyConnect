package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"v0.5.1", "v0.5.0", 1},
		{"v0.5.0", "v0.5.0", 0},
		{"v0.4.9", "v0.5.0", -1},
		{"v0.5.0", "v0.5.0-rc.1", 1},
		{"v0.5.0-rc.2", "v0.5.0-rc.1", 1},
	}
	for _, test := range tests {
		if got := compareVersions(test.left, test.right); got != test.want {
			t.Errorf("compareVersions(%q, %q)=%d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestParseVersionRejectsUnversionedBuild(t *testing.T) {
	if _, ok := parseVersion("dev"); ok {
		t.Fatal("dev should not parse as a release version")
	}
}

func TestUpdateStartReportsCommandFailure(t *testing.T) {
	err := runUpdateStart(exec.Command("sh", "-c", "echo 'sudo: permission denied' >&2; exit 1"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("failed command was accepted: %v", err)
	}
	if err := runUpdateStart(exec.Command("sh", "-c", "exit 0")); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateProgressPreservesInstalledVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if readUpdateProgress(path).State != "idle" {
		t.Fatal("missing status should be idle")
	}
	if err := os.WriteFile(path, []byte(`{"state":"failed","phase":"download","targetVersion":"v0.5.2","updatedAt":100,"currentVersion":"untrusted"}`), 0600); err != nil {
		t.Fatal(err)
	}
	progress := readUpdateProgress(path)
	if progress.State != "failed" || progress.Phase != "download" || progress.CurrentVersion != version || progress.UpdatedAt != 100 {
		t.Fatalf("unexpected progress: %+v", progress)
	}
	if err := os.WriteFile(path, []byte("invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if readUpdateProgress(path).State != "unknown" {
		t.Fatal("corrupt status should not report success")
	}
}

func TestUpdateProgressRequiresAuthentication(t *testing.T) {
	app := testApp(t, testConfig(t))
	server := httptest.NewServer(app.routes())
	defer server.Close()
	response := requestJSON(t, server.URL, "invalid", http.MethodGet, "/api/update/progress", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated progress returned %d", response.StatusCode)
	}
	response.Body.Close()
	response = requestJSON(t, server.URL, app.cfg.Auth.Password, http.MethodGet, "/api/update/progress", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated progress returned %d", response.StatusCode)
	}
}
