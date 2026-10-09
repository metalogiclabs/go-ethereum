// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph negative control for upstream help-wanted issue #35354.
// This is a diagnostic, NOT an implementation of ERA archive sync.
// It preserves the distinction "data in ERA" versus "data reachable through
// the fresh freezer's canonical-index/read API".
package rawdb

import (
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb/eradb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

func TestMathGraphFreshEraUnindexedBoundary(t *testing.T) {
	const blockNumber uint64 = 175881

	eraDir, err := filepath.Abs(filepath.Join("eradb", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	era, err := eradb.New(eraDir)
	if err != nil {
		t.Fatal(err)
	}
	defer era.Close()

	body, err := era.GetRawBody(blockNumber)
	if err != nil || len(body) == 0 {
		t.Fatalf("ERA body positive control: length=%d, err=%v", len(body), err)
	}
	receipts, err := era.GetRawReceipts(blockNumber)
	if err != nil || len(receipts) == 0 {
		t.Fatalf("ERA receipts positive control: length=%d, err=%v", len(receipts), err)
	}

	// A fresh archive-node data directory, with exactly the same working
	// ERA directory attached, has neither a populated freezer nor KV hashes.
	db, err := Open(memorydb.New(), OpenOptions{
		Ancient: filepath.Join(t.TempDir(), "ancients"),
		Era:     eraDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	frozen, err := db.Ancients()
	if err != nil {
		t.Fatal(err)
	}
	if frozen != 0 {
		t.Fatalf("fresh freezer unexpectedly contains %d ancient blocks", frozen)
	}

	if hash := ReadCanonicalHash(db, blockNumber); hash != (common.Hash{}) {
		t.Fatalf("unexpected canonical hash for the fresh freezer: %s", hash)
	}
	if got := ReadCanonicalBodyRLP(db, blockNumber, nil); len(got) != 0 {
		t.Fatalf("ERA body unexpectedly exposed without index: %d bytes", len(got))
	}
	if got := ReadCanonicalReceiptsRLP(db, blockNumber, nil); len(got) != 0 {
		t.Fatalf("ERA receipts unexpectedly exposed without index: %d bytes", len(got))
	}
	if got, err := db.Ancient(ChainFreezerBodiesTable, blockNumber); len(got) != 0 {
		t.Fatalf("fresh freezer unexpectedly exposed ERA data: %d bytes, err=%v", len(got), err)
	}
	t.Logf("INDEX_BOUNDARY_PRESENT: block %d has %d ERA body bytes and %d ERA receipts bytes; fresh freezer and canonical APIs return no data", blockNumber, len(body), len(receipts))
}
