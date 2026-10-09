// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Native backend qualification only. The fault is injected at the goleveldb
// storage journal writer (not at ethdb.Batch.Write or after DB.Write succeeds).
package leveldb

import (
	"errors"
	"sync/atomic"
	"testing"

	goleveldb "github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

var errInjectedJournalIO = errors.New("injected I/O error after all journal bytes were written")

// journalErrorStorage wraps real goleveldb Storage. An error can arise only
// from the journal Writer.Write operation itself, after its underlying writer
// accepted all the bytes. Thus the goleveldb.DB.Write call must decide whether
// to report an error despite the complete WAL record already being present.
type journalErrorStorage struct {
	storage.Storage
	armed           atomic.Bool
	injected        atomic.Bool
	syncBeforeError bool
}

func (s *journalErrorStorage) Create(fd storage.FileDesc) (storage.Writer, error) {
	writer, err := s.Storage.Create(fd)
	if err != nil {
		return nil, err
	}
	if fd.Type == storage.TypeJournal {
		return &journalFaultWriter{Writer: writer, store: s}, nil
	}
	return writer, nil
}

type journalFaultWriter struct {
	storage.Writer
	store *journalErrorStorage
}

func (w *journalFaultWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if err == nil && n == len(data) && w.store.armed.CompareAndSwap(true, false) {
		if w.store.syncBeforeError {
			if syncErr := w.Writer.Sync(); syncErr != nil {
				return n, syncErr
			}
		}
		w.store.injected.Store(true)
		return n, errInjectedJournalIO
	}
	return n, err
}

// TestNativeLevelDBReportedErrorAfterJournalBytes verifies the actual pinned
// goleveldb batch commit/recovery behavior. The storage is in-memory, so it
// checks the complete WAL record and recovery across DB handles, not fsync
// durability or spontaneous real-filesystem errors.
func TestNativeLevelDBReportedErrorAfterJournalBytes(t *testing.T) {
	storageBackend := &journalErrorStorage{Storage: storage.NewMemStorage()}
	db, err := goleveldb.Open(storageBackend, nil)
	if err != nil {
		t.Fatalf("open native LevelDB: %v", err)
	}

	batch := new(goleveldb.Batch)
	batch.Put([]byte("SnapshotSyncStatus"), []byte("next-pivot"))
	batch.Put([]byte("FlatState"), []byte("post-value"))
	storageBackend.armed.Store(true)
	err = db.Write(batch, nil)
	if !errors.Is(err, errInjectedJournalIO) {
		t.Fatalf("native LevelDB Write returned %v, want injected journal I/O error", err)
	}
	if !storageBackend.injected.Load() {
		t.Fatal("journal Storage.Writer.Write was not exercised")
	}
	if value, readErr := db.Get([]byte("SnapshotSyncStatus"), nil); !errors.Is(readErr, goleveldb.ErrNotFound) {
		t.Fatalf("batch unexpectedly applied in live memtable (value %q, err %v)", value, readErr)
	}
	if err := db.Close(); err != nil {
		t.Logf("close after injected I/O failure: %v", err)
	}

	reopened, err := goleveldb.Open(storageBackend, nil)
	if err != nil {
		t.Fatalf("reopen underlying storage: %v", err)
	}
	defer reopened.Close()
	for key, want := range map[string]string{
		"SnapshotSyncStatus": "next-pivot",
		"FlatState":          "post-value",
	} {
		actual, err := reopened.Get([]byte(key), nil)
		if err != nil || string(actual) != want {
			t.Fatalf("recovered native WAL %s = %q (err %v), want %q", key, actual, err, want)
		}
	}
}

// TestNativeLevelDBReportedErrorAfterSyncedFileJournal qualifies the same
// native WAL-writer error against actual file-backed storage. The injected
// journal Write error comes AFTER writing and fsyncing the complete record.
// Reopening through a new Storage handle must replay the next-pivot journal
// and corresponding state. This is still injected error behavior: it does
// not show that ordinary disks spontaneously return this exact outcome.
func TestNativeLevelDBReportedErrorAfterSyncedFileJournal(t *testing.T) {
	dir := t.TempDir()
	realStorage, err := storage.OpenFile(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	faultStore := &journalErrorStorage{Storage: realStorage, syncBeforeError: true}
	db, err := goleveldb.Open(faultStore, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Exercise geth's actual ethdb.Batch.Write adapter, which forwards
	// directly to the pinned goleveldb.DB.Write.
	b := (&Database{db: db}).NewBatch()
	if err := b.Put([]byte("SnapshotSyncStatus"), []byte("next-pivot")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put([]byte("FlatState"), []byte("new-flat-state")); err != nil {
		t.Fatal(err)
	}
	faultStore.armed.Store(true)
	if err := b.Write(); !errors.Is(err, errInjectedJournalIO) {
		t.Fatalf("geth ethdb Batch.Write: got %v, want native journal I/O error", err)
	}
	b.Close()
	if !faultStore.injected.Load() {
		t.Fatal("file-backed journal writer was not faulted")
	}
	if data, err := db.Get([]byte("SnapshotSyncStatus"), nil); !errors.Is(err, goleveldb.ErrNotFound) {
		t.Fatalf("live memtable unexpectedly advanced: %q, err %v", data, err)
	}
	if err := db.Close(); err != nil {
		t.Logf("close after file I/O fault: %v", err)
	}
	if err := realStorage.Close(); err != nil {
		t.Fatal(err)
	}

	// Distinct file Storage handle is closer to a process restart and forces
	// goleveldb to replay the WAL that already passed fsync.
	nextStorage, err := storage.OpenFile(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer nextStorage.Close()
	restarted, err := goleveldb.Open(nextStorage, nil)
	if err != nil {
		t.Fatalf("reopen synced WAL: %v", err)
	}
	defer restarted.Close()
	for key, want := range map[string]string{
		"SnapshotSyncStatus": "next-pivot",
		"FlatState":          "new-flat-state",
	} {
		data, err := restarted.Get([]byte(key), nil)
		if err != nil || string(data) != want {
			t.Fatalf("recovered %s = %q, err %v, want %q", key, data, err, want)
		}
	}
}
