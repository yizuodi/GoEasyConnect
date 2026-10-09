package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type releaseInfo struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
}

type updateStatus struct {
	CurrentVersion  string         `json:"currentVersion"`
	LatestVersion   string         `json:"latestVersion,omitempty"`
	UpdateAvailable bool           `json:"updateAvailable"`
	ReleaseURL      string         `json:"releaseUrl,omitempty"`
	ReleaseName     string         `json:"releaseName,omitempty"`
	PublishedAt     string         `json:"publishedAt,omitempty"`
	Enabled         bool           `json:"enabled"`
	Running         bool           `json:"running"`
	Error           string         `json:"error,omitempty"`
	Progress        updateProgress `json:"progress"`
}

type updateProgress struct {
	State          string `json:"state"`
	Phase          string `json:"phase,omitempty"`
	TargetVersion  string `json:"targetVersion,omitempty"`
	UpdatedAt      int64  `json:"updatedAt,omitempty"`
	CurrentVersion string `json:"currentVersion"`
	Error          string `json:"error,omitempty"`
}

const (
	updateRepository   = "yizuodi/GoEasyConnect"
	updateHelperPath   = "/usr/local/libexec/goeasyconnect-updater"
	updateUnitName     = "goeasyconnect-updater.service"
	updateProgressPath = "/var/lib/goeasyconnect-updater/status.json"
)

var semverPattern = regexp.MustCompile(`^[vV]([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?$`)

func (a *App) updateCheck(ctx context.Context) (updateStatus, error) {
	progress := a.getUpdateProgress()
	status := updateStatus{CurrentVersion: version, Enabled: a.cfg.Updates.Enabled, Running: progress.State == "running", Progress: progress}
	if !a.cfg.Updates.Enabled {
		return status, nil
	}
	if !semverPattern.MatchString(version) {
		return status, fmt.Errorf("current build version %q is not a release version", version)
	}
	repository := updateRepository
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return status, errors.New("updates.repository must use owner/name format")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repository+"/releases/latest", nil)
	if err != nil {
		return status, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "GoEasyConnect/"+version)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return status, fmt.Errorf("query GitHub Release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("GitHub Release returned HTTP %d", response.StatusCode)
	}
	var release releaseInfo
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return status, fmt.Errorf("decode GitHub Release: %w", err)
	}
	if release.Draft || release.TagName == "" {
		return status, errors.New("GitHub latest Release is unavailable")
	}
	status.LatestVersion = release.TagName
	status.ReleaseURL = release.HTMLURL
	status.ReleaseName = release.Name
	status.PublishedAt = release.PublishedAt
	status.UpdateAvailable = compareVersions(release.TagName, version) > 0
	return status, nil
}

func compareVersions(left, right string) int {
	leftParts, leftOK := parseVersion(left)
	rightParts, rightOK := parseVersion(right)
	if !leftOK || !rightOK {
		return strings.Compare(left, right)
	}
	for index := 0; index < 3; index++ {
		if leftParts.numbers[index] != rightParts.numbers[index] {
			if leftParts.numbers[index] < rightParts.numbers[index] {
				return -1
			}
			return 1
		}
	}
	if leftParts.prerelease == rightParts.prerelease {
		return 0
	}
	if leftParts.prerelease == "" {
		return 1
	}
	if rightParts.prerelease == "" {
		return -1
	}
	return strings.Compare(leftParts.prerelease, rightParts.prerelease)
}

type parsedVersion struct {
	numbers    [3]int
	prerelease string
}

func parseVersion(value string) (parsedVersion, bool) {
	matches := semverPattern.FindStringSubmatch(strings.TrimSpace(value))
	if matches == nil {
		return parsedVersion{}, false
	}
	var parsed parsedVersion
	for index := 0; index < 3; index++ {
		number, err := strconv.Atoi(matches[index+1])
		if err != nil {
			return parsedVersion{}, false
		}
		parsed.numbers[index] = number
	}
	parsed.prerelease = matches[4]
	return parsed, true
}

func (a *App) isUpdateRunning() bool {
	a.updateMu.Lock()
	running := a.updateRunning
	a.updateMu.Unlock()
	if running {
		return true
	}
	return a.getUpdateProgress().State == "running"
}

func (a *App) startUpdate() error {
	if !a.cfg.Updates.Enabled {
		return errors.New("updates are disabled")
	}
	if _, err := os.Stat(updateHelperPath); err != nil {
		return fmt.Errorf("update helper is unavailable: %w", err)
	}
	if a.getUpdateProgress().State == "running" {
		return errors.New("an update is already running")
	}
	a.updateMu.Lock()
	if a.updateRunning {
		a.updateMu.Unlock()
		return errors.New("an update is already running")
	}
	a.updateRunning = true
	a.updateMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sudo", "-n", "/usr/bin/systemctl", "start", "--no-block", updateUnitName)
	command.Dir = a.cfg.BaseDir
	defer func() {
		a.updateMu.Lock()
		a.updateRunning = false
		a.updateMu.Unlock()
	}()
	return runUpdateStart(command)
}

func runUpdateStart(command *exec.Cmd) error {
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("start update helper: %w: %s", err, strings.TrimSpace(truncateText(string(output), 2048)))
	}
	return nil
}

func readUpdateProgress(path string) updateProgress {
	progress := updateProgress{State: "idle", CurrentVersion: version}
	file, err := os.Open(path)
	if err != nil {
		return progress
	}
	defer file.Close()
	if err := json.NewDecoder(io.LimitReader(file, 4096)).Decode(&progress); err != nil {
		return updateProgress{State: "unknown", CurrentVersion: version, Error: "Cannot read updater status"}
	}
	progress.CurrentVersion = version
	return progress
}

func (a *App) getUpdateProgress() updateProgress {
	progress := readUpdateProgress(updateProgressPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--property=ActiveState", "--value", updateUnitName).Output()
	if err == nil {
		switch strings.TrimSpace(string(output)) {
		case "activating", "active":
			progress.State = "running"
		case "failed":
			progress.State = "failed"
			if progress.Error == "" {
				progress.Error = "Update failed; inspect journalctl -u goeasyconnect-updater.service -n 80 --no-pager"
			}
		case "inactive":
			if progress.State == "running" {
				progress.State = "failed"
				progress.Error = "Updater stopped before completion; inspect the updater journal"
			}
		}
	}
	return progress
}

func (a *App) handleUpdateProgress(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.getUpdateProgress())
}

func (a *App) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	status, err := a.updateCheck(ctx)
	if err != nil {
		status.Error = err.Error()
		writeJSON(w, http.StatusBadGateway, status)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *App) handleUpdateStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	status, err := a.updateCheck(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if !status.UpdateAvailable {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "EasyConnect is already up to date"})
		return
	}
	if err := a.startUpdate(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "status": "running"})
}
