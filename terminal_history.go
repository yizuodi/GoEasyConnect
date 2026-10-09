package main

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const terminalPreviewBytes = 128 * 1024

type terminalHistoryChunk struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type terminalHistoryPage struct {
	Chunks    []terminalHistoryChunk `json:"chunks"`
	Before    int64                  `json:"before"`
	End       int64                  `json:"end"`
	HasOlder  bool                   `json:"has_older"`
	Truncated bool                   `json:"truncated"`
}

// Page by rowid and byte offset, not message count: a single persisted terminal
// message may be several MiB. SQL only copies the requested BLOB suffix to Go.
func (s *Store) terminalHistory(ctx context.Context, sessionID string, before, end int64) (terminalHistoryPage, error) {
	page := terminalHistoryPage{Chunks: []terminalHistoryChunk{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	remaining := terminalPreviewBytes
	for count := 0; count < 50 && remaining > 0; count++ {
		var position, stop int64
		var chunk terminalHistoryChunk
		var content []byte
		err := tx.QueryRowContext(ctx, `WITH latest AS MATERIALIZED (
 SELECT rowid AS position,id,role,CAST(COALESCE(content,'') AS BLOB) AS body
 FROM messages WHERE session_id=? AND COALESCE(channel,'terminal')='terminal' AND (?=0 OR rowid<=?) ORDER BY rowid DESC LIMIT 1
), bounded AS (SELECT *,MIN(length(body),CASE WHEN position=? AND ?>0 THEN ? ELSE length(body) END) AS stop FROM latest)
 SELECT position,id,role,stop,substr(body,MAX(1,stop-?+1),MIN(?,stop)) FROM bounded`,
			sessionID, before, before, before, end, end, remaining, remaining).Scan(&position, &chunk.ID, &chunk.Role, &stop, &content)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return page, err
		}
		remaining -= len(content)
		start := stop - int64(len(content))
		if start > 0 {
			for len(content) > 0 && !utf8.RuneStart(content[0]) {
				content = content[1:]
				start++
			}
		}
		chunk.Content = strings.ToValidUTF8(string(content), "\uFFFD")
		page.Chunks = append(page.Chunks, chunk)
		if start > 0 {
			page.Before, page.End = position, start
			page.HasOlder, page.Truncated = true, true
			break
		}
		before, end = position-1, 0
		page.Before = before
		if before <= 0 {
			break
		}
	}
	if !page.HasOlder && page.Before > 0 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE session_id=? AND COALESCE(channel,'terminal')='terminal' AND rowid<=?)`, sessionID, page.Before).Scan(&exists); err != nil {
			return page, err
		}
		page.HasOlder = exists != 0
	}
	for i, j := 0, len(page.Chunks)-1; i < j; i, j = i+1, j-1 {
		page.Chunks[i], page.Chunks[j] = page.Chunks[j], page.Chunks[i]
	}
	return page, tx.Commit()
}

func (a *App) handleTerminalHistory(w http.ResponseWriter, r *http.Request) {
	if _, err := a.store.getSession(r.PathValue("id")); err != nil {
		if isNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Session not found"})
		} else {
			a.writeInternalError(w, "get terminal session", err)
		}
		return
	}
	parse := func(key string) (int64, error) {
		if r.URL.Query().Get(key) == "" {
			return 0, nil
		}
		return strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	}
	before, err := parse("before")
	end, endErr := parse("end")
	if err != nil || endErr != nil || before < 0 || end < 0 || (end > 0 && before == 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid history cursor"})
		return
	}
	page, err := a.store.terminalHistory(r.Context(), r.PathValue("id"), before, end)
	if err != nil {
		a.writeInternalError(w, "load terminal history", err)
		return
	}
	a.writeFeedJSON(w, r, page)
}

// Keep the suffix on a UTF-8 and escape-sequence boundary. State is scanned from
// the available ring's beginning so the cut cannot land inside CSI/OSC strings.
// Incomplete trailing sequences remain for live output, but not history replay.
func terminalTail(data []byte, maximum int, history bool) []byte {
	cut := len(data) - maximum
	if cut < 0 {
		cut = 0
	}
	state := byte(0)
	sequenceStart := 0
	start := -1
	end := len(data)
	for i, c := range data {
		if i >= cut && start < 0 && state == 0 && utf8.RuneStart(c) {
			start = i
		}
		switch state {
		case 0:
			if c == 0x1b {
				state = 1
				sequenceStart = i
			}
		case 1:
			switch c {
			case '[':
				state = 2
			case ']', 'P', '^', '_':
				state = 3
			default:
				state = 0
			}
		case 2:
			if c >= 0x40 && c <= 0x7e {
				state = 0
			}
		case 3:
			if c == 7 {
				state = 0
			} else if c == 0x1b {
				state = 4
			}
		case 4:
			if c == '\\' {
				state = 0
			} else if c != 0x1b {
				state = 3
			}
		}
	}
	if start < 0 {
		return nil
	}
	if history && state != 0 {
		// Discard the trailing escape fragment; do not leave xterm's parser open.
		end = sequenceStart
	}
	result := data[start:end]
	if history {
		return []byte(strings.ToValidUTF8(string(result), "\uFFFD"))
	}
	return append([]byte(nil), result...)
}
