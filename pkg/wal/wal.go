package wal

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Entry is a single WAL record
type Entry struct {
	Op         string
	Key        string
	Value      string
	TTLSeconds int64 // Add this field
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

// Append appends a new WAL entry and flushes immediately for durability.
func (w *WAL) Append(e Entry) error {
	if e.Op != "set" && e.Op != "delete" {
		return errors.New("invalid WAL entry op: " + e.Op)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	escapedValue := strings.ReplaceAll(e.Value, "\n", "\\n")

	// Include TTL in the WAL entry
	line := fmt.Sprintf("%s\t%s\t%s\t%d\n", e.Op, e.Key, escapedValue, e.TTLSeconds)
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

	scanner := bufio.NewScanner(w.f)
	var entries []Entry

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "\t", 4) // Changed to 4 to include TTL
		if len(parts) < 2 {
			continue // malformed line, skip
		}

		op := parts[0]
		key := parts[1]
		value := ""
		ttlSeconds := int64(0)

		if op == "set" {
			if len(parts) < 3 {
				continue
			}
			value = strings.ReplaceAll(parts[2], "\\n", "\n")
			if len(parts) >= 4 {
				if ttl, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
					ttlSeconds = ttl
				}
			}
		}

		if op != "set" && op != "delete" {
			continue // unknown op, skip
		}

		entries = append(entries, Entry{
			Op:         op,
			Key:        key,
			Value:      value,
			TTLSeconds: ttlSeconds,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.w.Flush(); err != nil {
		return err
	}
	return w.f.Close()
}
