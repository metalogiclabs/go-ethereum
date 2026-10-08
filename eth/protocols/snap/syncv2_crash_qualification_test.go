// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package snap

import (
	"bytes"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

type batchFaultMode uint8

const (
	batchFaultNone batchFaultMode = iota
	batchFaultPanicBeforeWrite
	batchFaultErrorAfterWrite
	batchFaultPanicAfterWrite
)

var errInjectedBatchWrite = errors.New("injected batch write failure")

// faultBatchDB injects a one-shot failure at the atomic batch boundary while
// leaving every other database operation untouched.
type faultBatchDB struct {
	ethdb.Database

	mu   sync.Mutex
	mode batchFaultMode
}

func (db *faultBatchDB) arm(mode batchFaultMode) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.mode = mode
}

func (db *faultBatchDB) takeFault() batchFaultMode {
	db.mu.Lock()
	defer db.mu.Unlock()
	mode := db.mode
	db.mode = batchFaultNone
	return mode
}

func (db *faultBatchDB) NewBatch() ethdb.Batch {
	return &faultBatch{Batch: db.Database.NewBatch(), db: db}
}

func (db *faultBatchDB) NewBatchWithSize(size int) ethdb.Batch {
	return &faultBatch{Batch: db.Database.NewBatchWithSize(size), db: db}
}

type faultBatch struct {
	ethdb.Batch
	db *faultBatchDB
}

func (b *faultBatch) Write() error {
	switch b.db.takeFault() {
	case batchFaultPanicBeforeWrite:
		panic("injected crash before batch write")
	case batchFaultErrorAfterWrite:
		if err := b.Batch.Write(); err != nil {
			return err
		}
		return errInjectedBatchWrite
	case batchFaultPanicAfterWrite:
		if err := b.Batch.Write(); err != nil {
			return err
		}
		panic("injected crash after durable batch write")
	default:
		return b.Batch.Write()
	}
}

// TestSnapV2CatchUpCrashAtomicity exercises the durable separator of BAL
// catch-up: state changes and the persisted pivot must move atomically. A
// restart must converge to the same root whether the process dies immediately
// before the batch write, immediately after it becomes durable, or the write
// commits but the caller receives an error.
func TestSnapV2CatchUpCrashAtomicity(t *testing.T) {
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		scheme := scheme
		t.Run(scheme, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				mode batchFaultMode
				useSync bool
			}{
				{name: "crash-before-write", mode: batchFaultPanicBeforeWrite},
				{name: "committed-write-reports-error", mode: batchFaultErrorAfterWrite, useSync: true},
				{name: "crash-after-durable-write", mode: batchFaultPanicAfterWrite},
			} {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					runCatchUpCrashQualification(t, scheme, tc.mode, tc.useSync)
				})
			}
		})
	}
}

func runCatchUpCrashQualification(t *testing.T, scheme string, mode batchFaultMode, useSync bool) {
	t.Helper()

	nodeScheme, sourceAccountTrie, elems, addrs := makeAccountTrieWithAddresses(100, scheme)
	rootA := sourceAccountTrie.Hash()
	numA := uint64(100)
	targetAddr := addrs[0]
	targetHash := crypto.Keccak256Hash(targetAddr[:])

	baseDB := rawdb.NewMemoryDatabase()
	db := &faultBatchDB{Database: baseDB}
	emptyHash := common.Hash{}
	zero := uint64(0)

	pivotA := &types.Header{
		Number: new(big.Int).SetUint64(numA), Root: rootA, Difficulty: common.Big0,
		BaseFee: common.Big0, WithdrawalsHash: &emptyHash,
		BlobGasUsed: &zero, ExcessBlobGas: &zero,
		ParentBeaconRoot: &emptyHash, RequestsHash: &emptyHash,
	}
	rawdb.WriteHeader(db, pivotA)
	rawdb.WriteCanonicalHash(db, pivotA.Hash(), numA)

	// Construct one canonical BAL transition A -> B.
	wantBalance := uint256.NewInt(424242)
	cb := bal.NewConstructionBlockAccessList()
	cb.BalanceChange(0, targetAddr, wantBalance)
	var buf bytes.Buffer
	if err := cb.EncodeRLP(&buf); err != nil {
		t.Fatal(err)
	}
	var decoded bal.BlockAccessList
	if err := rlp.DecodeBytes(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	balHash := decoded.Hash()
	nextLeaves := withLeafBalance(t, elems, targetHash, wantBalance.Uint64())
	pivotB := &types.Header{
		ParentHash: pivotA.Hash(), Root: accountTrieRoot(nextLeaves),
		Number: new(big.Int).SetUint64(numA + 1), Difficulty: common.Big0,
		BaseFee: common.Big0, WithdrawalsHash: &emptyHash,
		BlobGasUsed: &zero, ExcessBlobGas: &zero,
		ParentBeaconRoot: &emptyHash, RequestsHash: &emptyHash,
		BlockAccessListHash: &balHash,
	}
	rawdb.WriteHeader(db, pivotB)
	rawdb.WriteCanonicalHash(db, pivotB.Hash(), numA+1)
	bals := map[common.Hash]rlp.RawValue{pivotB.Hash(): bytes.Clone(buf.Bytes())}

	// Seed a complete sync at A. This makes catch-up maintain both flat state
	// and trie state, so the restart is checked against the canonical root.
	{
		var (
			once   sync.Once
			cancel = make(chan struct{})
			term   = func() { once.Do(func() { close(cancel) }) }
		)
		syncer := newSyncerV2(db, nodeScheme)
		src := newTestPeerV2("seed", t, term)
		src.accountTrie = sourceAccountTrie.Copy()
		src.accountValues = elems
		syncer.Register(src)
		src.remote = syncer
		if err := syncer.Sync(pivotA, cancel); err != nil {
			t.Fatalf("seed sync failed: %v", err)
		}
	}
	verifyTrie(scheme, db, pivotA.Root, t)

	// Inject the failure exactly at catch-up's atomic batch boundary.
	db.arm(mode)
	if useSync {
		var (
			once   sync.Once
			cancel = make(chan struct{})
			term   = func() { once.Do(func() { close(cancel) }) }
		)
		syncer := newSyncerV2(db, nodeScheme)
		src := newTestPeerV2("fault", t, term)
		src.accountTrie = sourceAccountTrie.Copy()
		src.accountValues = elems
		src.accessLists = bals
		syncer.Register(src)
		src.remote = syncer
		err := syncer.Sync(pivotB, cancel)
		if !errors.Is(err, errInjectedBatchWrite) {
			t.Fatalf("faulted sync error = %v, want %v", err, errInjectedBatchWrite)
		}
	} else {
		var (
			once   sync.Once
			cancel = make(chan struct{})
			term   = func() { once.Do(func() { close(cancel) }) }
		)
		syncer := newSyncerV2(db, nodeScheme)
		syncer.loadSyncStatus()
		src := newTestPeerV2("fault", t, term)
		src.accountTrie = sourceAccountTrie.Copy()
		src.accountValues = elems
		src.accessLists = bals
		syncer.Register(src)
		src.remote = syncer

		var wantPanic string
		switch mode {
		case batchFaultPanicBeforeWrite:
			wantPanic = "injected crash before batch write"
		case batchFaultPanicAfterWrite:
			wantPanic = "injected crash after durable batch write"
		default:
			t.Fatalf("unsupported direct catch-up fault mode %d", mode)
		}
		func() {
			defer func() {
				got := recover()
				if got != wantPanic {
					t.Fatalf("panic = %v, want %q", got, wantPanic)
				}
			}()
			_ = syncer.catchUp(pivotB, cancel)
		}()
	}

	// A fresh process must recover solely from durable state and converge to B.
	{
		var (
			once   sync.Once
			cancel = make(chan struct{})
			term   = func() { once.Do(func() { close(cancel) }) }
		)
		restarted := newSyncerV2(db, nodeScheme)
		src := newTestPeerV2("restart", t, term)
		src.accountTrie = sourceAccountTrie.Copy()
		src.accountValues = elems
		src.accessLists = bals
		restarted.Register(src)
		src.remote = restarted
		if err := restarted.Sync(pivotB, cancel); err != nil {
			t.Fatalf("restart sync failed after %v: %v", mode, err)
		}
	}

	loader := newSyncerV2(db, nodeScheme)
	loader.loadSyncStatus()
	if loader.pivot == nil || loader.pivot.Hash() != pivotB.Hash() {
		t.Fatalf("persisted pivot after restart = %v, want %v", loader.pivot, pivotB.Hash())
	}
	data := rawdb.ReadAccountSnapshot(db, targetHash)
	if len(data) == 0 {
		t.Fatal("target account missing after restart")
	}
	account, err := types.FullAccount(data)
	if err != nil {
		t.Fatalf("decode target account: %v", err)
	}
	if account.Balance.Cmp(wantBalance) != 0 {
		t.Fatalf("balance after restart = %v, want %v", account.Balance, wantBalance)
	}
	verifyTrie(scheme, db, pivotB.Root, t)
}
