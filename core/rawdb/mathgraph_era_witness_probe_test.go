// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Probe only: pin an explicit archive block witness before any canonical write.
package rawdb

import (
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb/eradb"
	"github.com/ethereum/go-ethereum/internal/era/onedb"
	"github.com/ethereum/go-ethereum/rlp"
)

func TestMathGraphEraWitnessProbe(t *testing.T) {
	const number uint64 = 175881
	eraFile := filepath.Join("eradb", "testdata", "sepolia-00021-b8814b14.era1")
	reader, err := onedb.Open(eraFile)
	if err != nil { t.Fatal(err) }
	defer reader.Close()
	block, err := reader.GetBlockByNumber(number)
	if err != nil { t.Fatal(err) }
	if block.NumberU64() != number { t.Fatalf("wrong era block number %d", block.NumberU64()) }
	store, err := eradb.New(filepath.Join("eradb", "testdata"))
	if err != nil { t.Fatal(err) }
	defer store.Close()
	body, err := store.GetRawBody(number)
	if err != nil { t.Fatal(err) }
	receipts, err := store.GetRawReceipts(number)
	if err != nil { t.Fatal(err) }
	encodedBody, err := rlp.EncodeToBytes(block.Body())
	if err != nil { t.Fatal(err) }
	if string(encodedBody) != string(body) { t.Fatal("archive header/body and raw-body access disagree") }
	t.Logf("WITNESS number=%d hash=%s parent=%s txs=%d body-sha256=%x receipts-storage-sha256=%x", number, block.Hash().Hex(), block.ParentHash().Hex(), len(block.Transactions()), sha256.Sum256(body), sha256.Sum256(receipts))
}
