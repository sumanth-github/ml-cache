package wal

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Entry is a single WAL record
type Entry struct {
	Key   string
	Value string
}

// WAL is a tiny append-only writer
type WAL struct {
	f  *os.File
	w  *bufio.Writer
	mu sync.Mutex
}

func NewWAL(path string) (*WAL, error) {
	if err := os.MkdirAll("./data", 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	return &WAL{f: f, w: bufio.NewWriter(f)}, nil
}

func (w *WAL) Append(e Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := fmt.Sprintf("%s\t%s\n", e.Key, strings.ReplaceAll(e.Value, "\n", "\\n"))
	if _, err := w.w.WriteString(line); err != nil {
		return err
	}
	return w.w.Flush()
}

func (w *WAL) ReadAll() ([]Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Seek(0, 0); err != nil {
		return nil, err
	}
	s := bufio.NewScanner(w.f)
	var out []Entry
	for s.Scan() {
		line := s.Text()
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		out = append(out, Entry{
			Key:   parts[0],
			Value: strings.ReplaceAll(parts[1], "\\n", "\n"),
		})
	}
	return out, nil
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.f.Close()
}
