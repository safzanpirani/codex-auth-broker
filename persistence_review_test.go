package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func reviewLog(t *testing.T, maxBytes int64) *requestLogFile {
	t.Helper()
	f, err := openRequestLogFileWithLimit(filepath.Join(t.TempDir(), "requests.jsonl"), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f.file != nil {
			_ = f.file.Close()
		}
	})
	return f
}

func reviewEntry(id int64) requestLogEntry {
	return requestLogEntry{ID: id, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Method: "POST", Path: "/v1/responses", Model: "gpt-6-astra", Status: 200}
}

func TestReviewRequestLogCapKeepsNewestWholeEntries(t *testing.T) {
	const capBytes = 4096
	f := reviewLog(t, capBytes)
	for i := int64(1); i <= 150; i++ {
		if err := f.append(reviewEntry(i)); err != nil {
			t.Fatal(err)
		}
		stat, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Size() > capBytes {
			t.Fatalf("size %d exceeds cap %d", stat.Size(), capBytes)
		}
	}
	entries, maxID, err := loadPersistedEntries(f.path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 || entries[0].ID <= 1 || maxID != 150 || entries[len(entries)-1].ID != 150 {
		t.Fatalf("retained %d entries, maxID=%d", len(entries), maxID)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].ID != entries[i-1].ID+1 {
			t.Fatal("compaction lost ordering or a whole recent entry")
		}
	}
	if runtime.GOOS != "windows" {
		stat, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Mode().Perm() != 0o600 {
			t.Fatalf("compaction mode = %o", stat.Mode().Perm())
		}
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".requests-*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left after compaction: %d, error %v", len(leftovers), err)
	}
}

func TestReviewRequestLogCompactsAtOpenAndSupportsUnlimited(t *testing.T) {
	f := reviewLog(t, 0)
	for i := int64(1); i <= 100; i++ {
		if err := f.append(reviewEntry(i)); err != nil {
			t.Fatal(err)
		}
	}
	stat, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() <= 4096 {
		t.Fatal("unlimited file did not grow past test cap")
	}
	if err := f.file.Close(); err != nil {
		t.Fatal(err)
	}
	f.file = nil
	bounded, err := openRequestLogFileWithLimit(f.path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer bounded.file.Close()
	stat, err = os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() > 4096 {
		t.Fatalf("startup left %d bytes", stat.Size())
	}
	entries, maxID, err := loadPersistedEntries(f.path, 1000)
	if err != nil || len(entries) == 0 || maxID != 100 {
		t.Fatalf("startup retained=%d maxID=%d err=%v", len(entries), maxID, err)
	}
}

func TestReviewRequestLogRetainsEntryExactlyAtCap(t *testing.T) {
	f := reviewLog(t, 0)
	old := reviewEntry(1)
	newest := reviewEntry(2)
	encoded, err := json.Marshal(sanitizeRequestLogEntry(newest))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []requestLogEntry{old, newest} {
		if err := f.append(entry); err != nil {
			t.Fatal(err)
		}
	}
	f.maxBytes = int64(len(encoded) + 1)
	if err := f.compact(nil); err != nil {
		t.Fatal(err)
	}
	entries, maxID, err := loadPersistedEntries(f.path, 10)
	if err != nil || len(entries) != 1 || maxID != newest.ID {
		t.Fatalf("exact cap retained=%d maxID=%d err=%v", len(entries), maxID, err)
	}
}

func TestReviewRequestLogRejectsOversizedEntryWithoutLosingHistory(t *testing.T) {
	f := reviewLog(t, 512)
	if err := f.append(reviewEntry(1)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	entry := reviewEntry(2)
	entry.Model, entry.RequestID, entry.Error = strings.Repeat("text ", 60), strings.Repeat("id ", 100), strings.Repeat("error ", 60)
	if err := f.append(entry); err == nil {
		t.Fatal("oversized entry accepted")
	}
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("oversized entry changed previous file")
	}
}

type reviewPartialWriter struct {
	requestLogHandle
	short bool
}

func (w reviewPartialWriter) Write(p []byte) (int, error) {
	n, err := w.requestLogHandle.Write(p[:len(p)/2])
	if err != nil || w.short {
		return n, err
	}
	return n, errors.New("synthetic writer failure with private detail")
}

func TestReviewRequestLogWriteFailureRollsBackAndReportsDegradation(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "write-error", true: "short-write"}[short], func(t *testing.T) {
			f := reviewLog(t, 4096)
			store := newRequestLogStore(10)
			store.persist = f
			store.add(reviewEntry(1))
			before, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			original := f.file
			f.file = reviewPartialWriter{requestLogHandle: original, short: short}
			store.add(reviewEntry(2))
			after, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed append changed committed history")
			}
			snapshot := store.snapshot(10)
			if snapshot.Retained != 2 || snapshot.PersistError == "" {
				t.Fatal("failed append lost in-memory entry or did not report degraded persistence")
			}
			if strings.Contains(snapshot.PersistError, "private detail") {
				t.Fatal("persistence health leaked raw error")
			}
			f.file = original
			store.add(reviewEntry(3))
			entries, maxID, err := loadPersistedEntries(f.path, 10)
			if err != nil || len(entries) != 2 || maxID != 3 {
				t.Fatalf("recovery entries=%d maxID=%d err=%v", len(entries), maxID, err)
			}
			if store.snapshot(10).PersistError == "" {
				t.Fatal("recovery hid the missing historical entry")
			}
			costs, err := store.costSummary(time.Now(), false)
			if err != nil || costs.PersistError == "" {
				t.Fatal("cost summary omitted degraded persistence")
			}
			if err := store.clear(); err != nil {
				t.Fatal(err)
			}
			if store.snapshot(10).PersistError != "" {
				t.Fatal("clear did not reset persistence warning")
			}
		})
	}
}

func TestReviewRequestLogFailedReplacementKeepsOriginalAndReopens(t *testing.T) {
	f := reviewLog(t, 4096)
	if err := f.append(reviewEntry(1)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.replaceWith(filepath.Join(filepath.Dir(f.path), "absent-temp")); err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed replacement changed original")
	}
	if err := f.append(reviewEntry(2)); err != nil {
		t.Fatalf("append after failed replacement: %v", err)
	}
}

func TestReviewRequestLogRepairsInterruptedTail(t *testing.T) {
	f := reviewLog(t, 4096)
	if err := f.append(reviewEntry(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.file.Write([]byte(`{"id":2,"model":"interrupted`)); err != nil {
		t.Fatal(err)
	}
	if err := f.append(reviewEntry(3)); err != nil {
		t.Fatal(err)
	}
	entries, maxID, err := loadPersistedEntries(f.path, 10)
	if err != nil || len(entries) != 2 || maxID != 3 {
		t.Fatalf("tail recovery entries=%d maxID=%d err=%v", len(entries), maxID, err)
	}
}

func TestReviewRequestLogRingRestoreOrdering(t *testing.T) {
	f := reviewLog(t, 0)
	for i := int64(1); i <= 27; i++ {
		if err := f.append(reviewEntry(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{1, 3, 4, 9, 30} {
		entries, maxID, err := loadPersistedEntries(f.path, limit)
		if err != nil || len(entries) != min(limit, 27) || maxID != 27 {
			t.Fatalf("limit=%d retained=%d maxID=%d err=%v", limit, len(entries), maxID, err)
		}
		for i, entry := range entries {
			if entry.ID != int64(28-len(entries)+i) {
				t.Fatalf("limit %d returned incorrect order", limit)
			}
		}
	}
}

func TestReviewRequestLogConcurrentCompactionAndAggregation(t *testing.T) {
	f := reviewLog(t, 4096)
	store := newRequestLogStore(10)
	store.persist = f
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			store.add(reviewEntry(int64(i)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := store.costSummary(time.Now(), false); err != nil {
				t.Errorf("concurrent cost summary: %v", err)
			}
		}
	}()
	wg.Wait()
	if err := store.snapshot(10).PersistError; err != "" {
		t.Fatalf("concurrent persistence warning: %s", err)
	}
}

func TestReviewTruncateLogFieldRespectsByteCap(t *testing.T) {
	for _, size := range []int{1, 2, 3, 4, 16, 64, 128, 256} {
		if got := truncateLogField(strings.Repeat("normal text ", 100), size); len(got) > size {
			t.Fatalf("limit=%d result bytes=%d", size, len(got))
		}
	}
}

func TestReviewRequestLogPreservesLegacyFinalRecordWithoutNewline(t *testing.T) {
	f := reviewLog(t, 4096)
	entry, err := json.Marshal(reviewEntry(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.file.Write(entry); err != nil {
		t.Fatal(err)
	}
	if err := f.append(reviewEntry(2)); err != nil {
		t.Fatal(err)
	}
	entries, maxID, err := loadPersistedEntries(f.path, 10)
	if err != nil || len(entries) != 2 || entries[0].ID != 1 || maxID != 2 {
		t.Fatalf("newline repair retained=%d maxID=%d err=%v", len(entries), maxID, err)
	}
}

type reviewCloseFailure struct{ requestLogHandle }

func (f reviewCloseFailure) Close() error {
	_ = f.requestLogHandle.Close()
	return errors.New("synthetic close failure")
}

func TestReviewRequestLogCompactionFailurePreservesFileAndCleansTemp(t *testing.T) {
	f := reviewLog(t, 4096)
	for i := int64(1); i <= 5; i++ {
		if err := f.append(reviewEntry(i)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.file = reviewCloseFailure{f.file}
	if err := f.compact(nil); err == nil {
		t.Fatal("compaction unexpectedly succeeded")
	}
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed compaction changed original")
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".requests-*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left after failed compaction: %d, error %v", len(leftovers), err)
	}
	if err := f.append(reviewEntry(6)); err != nil {
		t.Fatalf("append did not recover: %v", err)
	}
}

type reviewCountingReader struct {
	io.ReaderAt
	bytesRead int
}

func (r *reviewCountingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	r.bytesRead += n
	return n, err
}

func TestReviewRequestLogTailReaderBoundsOversizedHistory(t *testing.T) {
	newest, err := json.Marshal(reviewEntry(1000))
	if err != nil {
		t.Fatal(err)
	}
	payload := append(bytes.Repeat([]byte("malformed old metadata\n"), 100_000), append(newest, '\n')...)
	counted := &reviewCountingReader{ReaderAt: bytes.NewReader(payload)}
	reader, err := requestLogTailReader(counted, int64(len(payload)), 4096)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	if err := scanRequestLogReader(reader, func(entry requestLogEntry) { ids = append(ids, entry.ID) }); err != nil {
		t.Fatal(err)
	}
	if counted.bytesRead > 4097 || len(ids) != 1 || ids[0] != 1000 {
		t.Fatalf("tail read bytes=%d entries=%v", counted.bytesRead, ids)
	}
}

func TestReviewRequestLogFailedCompactionStillBoundsRestoreAndCosts(t *testing.T) {
	f := reviewLog(t, 0)
	for i := int64(1); i <= 100; i++ {
		if err := f.append(reviewEntry(i)); err != nil {
			t.Fatal(err)
		}
	}
	f.maxBytes = 4096
	f.file = reviewCloseFailure{f.file}
	if err := f.compact(nil); err == nil {
		t.Fatal("compaction unexpectedly succeeded")
	}
	entries, maxID, err := f.loadEntries(1000)
	if err != nil || len(entries) == 0 || len(entries) >= 100 || maxID != 100 {
		t.Fatalf("bounded restore retained=%d maxID=%d err=%v", len(entries), maxID, err)
	}
	store := newRequestLogStore(1000)
	store.persist = f
	summary, err := store.costSummary(time.Now(), false)
	if err != nil || summary.Windows["all"].Requests != len(entries) {
		t.Fatalf("bounded costs count=%d want=%d err=%v", summary.Windows["all"].Requests, len(entries), err)
	}
}
