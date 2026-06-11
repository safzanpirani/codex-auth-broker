package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// requestLogFile appends redacted request metadata as JSONL. It only ever
// receives requestLogEntry values, which by construction contain no prompt
// text, completion text, request bodies, or credentials.
type requestLogFile struct {
	path string
	file *os.File
}

func openRequestLogFile(path string) (*requestLogFile, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o700); err != nil {
		return nil, fmt.Errorf("create request log directory: %w", err)
	}
	file, err := os.OpenFile(expanded, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open request log file: %w", err)
	}
	return &requestLogFile{path: expanded, file: file}, nil
}

// append writes one entry as a JSON line. Called with the store mutex held.
func (f *requestLogFile) append(entry requestLogEntry) {
	if f == nil || f.file == nil {
		return
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = f.file.Write(append(encoded, '\n'))
}

// scanPersistedEntries streams the JSONL file and calls fn for each parsed
// entry. Malformed lines are skipped; a missing file is not an error.
func scanPersistedEntries(path string, fn func(requestLogEntry)) error {
	expanded, err := expandPath(path)
	if err != nil {
		return err
	}
	file, err := os.Open(expanded)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry requestLogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		fn(entry)
	}
	return scanner.Err()
}

// loadPersistedEntries returns the last limit entries plus the highest ID
// seen, so in-memory history and ID numbering survive restarts.
func loadPersistedEntries(path string, limit int) ([]requestLogEntry, int64, error) {
	var entries []requestLogEntry
	var maxID int64
	err := scanPersistedEntries(path, func(entry requestLogEntry) {
		if entry.ID > maxID {
			maxID = entry.ID
		}
		entries = append(entries, entry)
		if limit > 0 && len(entries) > limit {
			copy(entries, entries[1:])
			entries = entries[:limit]
		}
	})
	if err != nil {
		return nil, 0, err
	}
	return entries, maxID, nil
}
