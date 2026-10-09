// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// A native Pebble pre-commit error control. The production adapter uses
// pebble.NoSync; this test deliberately exercises the stable read-only error
// rather than inventing an error after a successful underlying Commit.
package pebble

import (
	"errors"
	"testing"

	pebblev2 "github.com/cockroachdb/pebble/v2"
)

func TestNativePebbleReadOnlyWriteErrorDoesNotAdvanceState(t *testing.T) {
	location := t.TempDir()
	seed, err := New(location, 16, 16, "native-write-error-probe", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Put([]byte("pivot"), []byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := New(location, 16, 16, "native-write-error-probe-readonly", true)
	if err != nil {
		t.Fatal(err)
	}
	batch := readOnly.NewBatch()
	if err := batch.Put([]byte("pivot"), []byte("B")); err != nil {
		t.Fatal(err)
	}
	if err := batch.Put([]byte("state"), []byte("new-state")); err != nil {
		t.Fatal(err)
	}
	err = batch.Write()
	if !errors.Is(err, pebblev2.ErrReadOnly) {
		t.Fatalf("Pebble Batch.Write returned %v; want pre-commit ErrReadOnly", err)
	}
	batch.Close()
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(location, 16, 16, "native-write-error-probe-reopen", false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	value, err := reopened.Get([]byte("pivot"))
	if err != nil || string(value) != "A" {
		t.Fatalf("after failed Pebble batch, pivot = %q, err %v; want A", value, err)
	}
	if exists, err := reopened.Has([]byte("state")); err != nil || exists {
		t.Fatalf("failed Pebble batch persisted state key? %v, err %v", exists, err)
	}
}
