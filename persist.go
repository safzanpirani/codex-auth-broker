package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const defaultRequestLogMaxBytes int64 = 64 * 1024 * 1024

type requestLogHandle interface {
	io.ReaderAt
	io.Writer
	io.Seeker
	Stat() (os.FileInfo, error)
	Truncate(int64) error
	Sync() error
	Close() error
}

// requestLogFile appends redacted request metadata as JSONL. It only ever
// receives requestLogEntry values, which by construction contain no prompt
// text, completion text, request bodies, or credentials.
type requestLogFile struct {
	mu        sync.RWMutex
	path      string
	file      requestLogHandle
	maxBytes  int64
	lastError string
}

func openRequestLogFile(path string) (*requestLogFile, error) {
	return openRequestLogFileWithLimit(path, defaultRequestLogMaxBytes)
}

func openRequestLogFileWithLimit(path string, maxBytes int64) (*requestLogFile, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("request log size limit must be nonnegative")
	}
	expanded, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o700); err != nil {
		return nil, fmt.Errorf("create request log directory: %w", err)
	}
	file, err := os.OpenFile(expanded, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open request log file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure request log file: %w", err)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("seek request log file: %w", err)
	}
	f := &requestLogFile{path: expanded, file: file, maxBytes: maxBytes}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat request log file: %w", err)
	}
	if maxBytes > 0 && stat.Size() > maxBytes {
		if err := f.compact(nil); err != nil {
			f.lastError = "request log compaction failed; retained history may exceed its size limit"
		}
	}
	return f, nil
}

// append writes one entry as a JSON line. Called with the store mutex held.
// Failed writes keep previous complete entries and never fail inference. The
// warning stays latched until history is cleared because a later successful
// write cannot recover an entry that was missed.
func (f *requestLogFile) append(entry requestLogEntry) (err error) {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() {
		if err != nil {
			f.lastError = "request history may be incomplete: persistent log write failed"
		}
	}()
	if f.file == nil {
		return fmt.Errorf("request log file is unavailable")
	}
	encoded, err := json.Marshal(sanitizeRequestLogEntry(entry))
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if f.maxBytes > 0 && int64(len(encoded)) > f.maxBytes {
		return fmt.Errorf("request log entry exceeds size limit")
	}
	needsNewline, err := f.repairTail()
	if err != nil {
		return err
	}
	start, err := f.file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	appendBytes := int64(len(encoded))
	if needsNewline {
		appendBytes++
	}
	if f.maxBytes > 0 && start > f.maxBytes-appendBytes {
		return f.compact(encoded)
	}
	if needsNewline {
		encoded = append([]byte{'\n'}, encoded...)
	}
	n, err := f.file.Write(encoded)
	if err == nil && n == len(encoded) {
		return nil
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	if rollbackErr := f.file.Truncate(start); rollbackErr != nil {
		return fmt.Errorf("append request log failed: %w; rollback failed: %v", err, rollbackErr)
	}
	return err
}

// repairTail discards an interrupted final line before appending. Valid legacy
// records without a final newline are preserved; the caller adds the separator
// as part of its append transaction. Backward scanning uses bounded memory.
func (f *requestLogFile) repairTail() (bool, error) {
	stat, err := f.file.Stat()
	if err != nil || stat.Size() == 0 {
		return false, err
	}
	end := stat.Size()
	var last [1]byte
	if _, err := f.file.ReadAt(last[:], end-1); err != nil {
		return false, err
	}
	if last[0] == '\n' {
		return false, nil
	}
	buf := make([]byte, 64*1024)
	tailStart := int64(0)
search:
	for end > 0 {
		start := max(int64(0), end-int64(len(buf)))
		chunk := buf[:end-start]
		if _, err := f.file.ReadAt(chunk, start); err != nil {
			return false, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				tailStart = start + int64(i) + 1
				break search
			}
		}
		end = start
	}
	if length := stat.Size() - tailStart; length <= 1024*1024 {
		tail, err := io.ReadAll(io.NewSectionReader(f.file, tailStart, length))
		if err != nil {
			return false, err
		}
		var entry requestLogEntry
		if json.Unmarshal(tail, &entry) == nil {
			return true, nil
		}
	}
	if err := f.file.Truncate(tailStart); err != nil {
		return false, err
	}
	f.lastError = "request history may be incomplete: interrupted log entry was discarded"
	return false, nil
}

// compact prepares a protected replacement without touching the existing file.
// Only the newest whole metadata entries are kept, with headroom for appends.
// The read and retained queue are bounded by maxBytes, including on startup.
func (f *requestLogFile) compact(extra []byte) error {
	stat, err := f.file.Stat()
	if err != nil {
		return err
	}
	reader, err := requestLogTailReader(f.file, stat.Size(), f.maxBytes)
	if err != nil {
		return err
	}
	target := f.maxBytes - f.maxBytes/4
	var kept [][]byte
	head := 0
	var size int64
	var encodeErr error
	keep := func(line []byte) {
		if int64(len(line)) > f.maxBytes {
			encodeErr = fmt.Errorf("request log entry exceeds size limit")
			return
		}
		kept = append(kept, line)
		size += int64(len(line))
		for size > target && len(kept)-head > 1 {
			size -= int64(len(kept[head]))
			kept[head] = nil
			head++
		}
		if head > 1024 && head*2 >= len(kept) {
			kept = append(kept[:0], kept[head:]...)
			head = 0
		}
	}
	if err := scanRequestLogReader(reader, func(entry requestLogEntry) {
		line, err := json.Marshal(entry)
		if err != nil {
			encodeErr = err
			return
		}
		keep(append(line, '\n'))
	}); err != nil {
		return err
	}
	if extra != nil {
		keep(extra)
	}
	if encodeErr != nil {
		return encodeErr
	}
	if len(kept) == head && stat.Size() > 0 && extra == nil {
		return fmt.Errorf("no complete request metadata fits within the size limit")
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".requests-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	for _, line := range kept[head:] {
		if _, err := tmp.Write(line); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return f.replaceWith(tmp.Name())
}

// requestLogTailReader bounds reads even when compaction cannot replace an
// oversized file. Zero opts into unlimited history. A partial leading line is
// skipped, so callers only observe whole retained metadata entries.
func requestLogTailReader(file io.ReaderAt, size, maxBytes int64) (io.Reader, error) {
	start := int64(0)
	if maxBytes > 0 {
		start = max(int64(0), size-maxBytes)
	}
	reader := bufio.NewReader(io.NewSectionReader(file, start, size-start))
	if start > 0 {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], start-1); err != nil {
			return nil, err
		}
		for previous[0] != '\n' {
			_, err := reader.ReadSlice('\n')
			if err != bufio.ErrBufferFull {
				if err != nil && err != io.EOF {
					return nil, err
				}
				break
			}
		}
	}
	return reader, nil
}

// replaceWith closes handles before rename for Windows. A failed rename keeps
// the original path intact and reopens it so subsequent appends can recover.
func (f *requestLogFile) replaceWith(path string) error {
	if err := f.file.Close(); err != nil {
		f.file = nil
		if reopened, openErr := os.OpenFile(f.path, os.O_RDWR, 0o600); openErr == nil {
			f.file = reopened
		}
		return err
	}
	f.file = nil
	err := os.Rename(path, f.path)
	file, openErr := os.OpenFile(f.path, os.O_RDWR, 0o600)
	if openErr == nil {
		f.file = file
	}
	if err != nil {
		return err
	}
	if openErr != nil {
		return openErr
	}
	return syncDir(filepath.Dir(f.path))
}

// clear truncates persisted history while preserving the open append handle.
// Called with the store mutex held.
func (f *requestLogFile) clear() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return fmt.Errorf("request log file is unavailable")
	}
	if err := f.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate request log: %w", err)
	}
	if _, err := f.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek request log: %w", err)
	}
	if err := f.file.Sync(); err != nil {
		return fmt.Errorf("sync request log: %w", err)
	}
	f.lastError = ""
	return nil
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
	return scanRequestLogReader(file, fn)
}

func scanRequestLogReader(input io.Reader, fn func(requestLogEntry)) error {
	const maxEntryBytes = 1024 * 1024
	reader := bufio.NewReaderSize(input, 64*1024)
	line := make([]byte, 0, 64*1024)
	oversized := false
	for {
		fragment, isPrefix, readErr := reader.ReadLine()
		if !oversized && len(fragment) > 0 {
			if len(line)+len(fragment) > maxEntryBytes {
				line = nil
				oversized = true
			} else {
				line = append(line, fragment...)
			}
		}
		if !isPrefix {
			if !oversized && len(line) > 0 {
				var entry requestLogEntry
				if json.Unmarshal(line, &entry) == nil {
					fn(sanitizeRequestLogEntry(entry))
				}
			}
			line = line[:0]
			oversized = false
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// loadPersistedEntries returns the last limit entries plus the highest ID
// seen, so in-memory history and ID numbering survive restarts.
func loadPersistedEntries(path string, limit int) ([]requestLogEntry, int64, error) {
	if limit <= 0 {
		return nil, 0, nil
	}
	return collectRequestLogEntries(limit, func(fn func(requestLogEntry)) error {
		return scanPersistedEntries(path, fn)
	})
}

// loadEntries restores only the configured retained tail, including when an
// oversized file could not be compacted. It never rewrites failed history.
func (f *requestLogFile) loadEntries(limit int) ([]requestLogEntry, int64, error) {
	if f == nil || limit <= 0 {
		return nil, 0, nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	file, err := os.Open(f.path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	reader, err := requestLogTailReader(file, stat.Size(), f.maxBytes)
	if err != nil {
		return nil, 0, err
	}
	return collectRequestLogEntries(limit, func(fn func(requestLogEntry)) error {
		return scanRequestLogReader(reader, fn)
	})
}

func collectRequestLogEntries(limit int, scan func(func(requestLogEntry)) error) ([]requestLogEntry, int64, error) {
	var entries []requestLogEntry
	var maxID int64
	next := 0
	err := scan(func(entry requestLogEntry) {
		if entry.ID > maxID {
			maxID = entry.ID
		}
		if len(entries) < limit {
			entries = append(entries, entry)
		} else {
			entries[next] = entry
			next = (next + 1) % limit
		}
	})
	if err != nil {
		return nil, 0, err
	}
	if next > 0 {
		ordered := make([]requestLogEntry, 0, len(entries))
		ordered = append(ordered, entries[next:]...)
		entries = append(ordered, entries[:next]...)
	}
	return entries, maxID, nil
}

type requestHistorySource struct {
	Source       string
	PersistPath  string
	PersistBytes int64
	PersistError string
}

// visitRetainedEntries gives aggregators the same source and current-price
// semantics. File readers exclude compaction/clear for Windows compatibility.
func (s *requestLogStore) visitRetainedEntries(fn func(requestLogEntry)) (requestHistorySource, error) {
	source := requestHistorySource{Source: "memory"}
	if s == nil {
		return source, nil
	}
	s.mu.Lock()
	if f := s.persist; f != nil {
		f.mu.RLock()
		source.Source, source.PersistPath, source.PersistError = "file", f.path, f.lastError
		s.mu.Unlock()
		defer f.mu.RUnlock()
		file, err := os.Open(f.path)
		if err != nil {
			return source, err
		}
		defer file.Close()
		stat, err := file.Stat()
		if err != nil {
			return source, err
		}
		source.PersistBytes = stat.Size()
		reader, err := requestLogTailReader(file, stat.Size(), f.maxBytes)
		if err != nil {
			return source, err
		}
		return source, scanRequestLogReader(reader, func(entry requestLogEntry) {
			fn(s.priceEntry(entry))
		})
	}
	entries := append([]requestLogEntry(nil), s.entries...)
	s.mu.Unlock()
	for _, entry := range entries {
		fn(s.priceEntry(entry))
	}
	return source, nil
}
