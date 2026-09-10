package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedLogWriterCompactsInPlace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "error.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := &boundedLogWriter{file: file, path: path, maxBytes: 64}
	for index := 0; index < 10; index++ {
		if _, err := writer.Write([]byte("line-abcdefghijk\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 80 {
		t.Fatalf("bounded log grew to %d bytes", info.Size())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(content), "\n") {
		t.Fatalf("compacted log is not line-aligned: %q", content)
	}
}

func TestBoundedLogWriterTruncatesOversizedEntry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "error.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := &boundedLogWriter{file: file, path: path, maxBytes: 32}
	entry := strings.Repeat("x", 100)
	written, err := writer.Write([]byte(entry))
	if err != nil {
		t.Fatal(err)
	}
	if written != len(entry) {
		t.Fatalf("Write reported %d bytes, want %d", written, len(entry))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 32 || string(content) != strings.Repeat("x", 32) {
		t.Fatalf("oversized log content=%q len=%d", content, len(content))
	}
}
