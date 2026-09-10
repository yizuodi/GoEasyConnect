package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

type boundedLogWriter struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	maxBytes int64
}

func newErrorLogger(path string, maxSizeMB int64) (*log.Logger, io.Closer, error) {
	if err := os.MkdirAll(filepathDir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open error log: %w", err)
	}
	_ = file.Chmod(0o600)
	writer := &boundedLogWriter{file: file, path: path, maxBytes: maxSizeMB * 1024 * 1024}
	return log.New(writer, "", log.LstdFlags|log.LUTC), writer, nil
}

func (w *boundedLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes <= 0 {
		return w.file.Write(data)
	}
	if int64(len(data)) >= w.maxBytes {
		if err := w.retainTail(0); err != nil {
			return 0, err
		}
		if _, err := w.file.Write(data[len(data)-int(w.maxBytes):]); err != nil {
			return 0, err
		}
		return len(data), nil
	}
	if info, err := w.file.Stat(); err != nil {
		return 0, err
	} else if info.Size()+int64(len(data)) > w.maxBytes {
		if err := w.retainTail(w.maxBytes - int64(len(data))); err != nil {
			return 0, err
		}
	}
	return w.file.Write(data)
}

func (w *boundedLogWriter) retainTail(keep int64) error {
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	start := info.Size() - keep
	if start < 0 {
		start = 0
	}
	buf := make([]byte, int(info.Size()-start))
	if _, err := w.file.ReadAt(buf, start); err != nil && err != io.EOF {
		return err
	}
	if err := w.file.Truncate(0); err != nil {
		return err
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if start > 0 {
		if newline := firstNewline(buf); newline >= 0 {
			buf = buf[newline+1:]
		}
	}
	_, err = w.file.Write(buf)
	return err
}

func (w *boundedLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

func firstNewline(data []byte) int {
	for index, value := range data {
		if value == '\n' {
			return index
		}
	}
	return -1
}

func filepathDir(path string) string {
	index := len(path) - 1
	for index >= 0 && path[index] != '/' {
		index--
	}
	if index <= 0 {
		return "."
	}
	return path[:index]
}
