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
	armed    atomic.Bool
	injected atomic.Bool
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
