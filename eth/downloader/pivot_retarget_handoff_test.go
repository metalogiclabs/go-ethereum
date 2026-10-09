// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package downloader

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// pivotAuditSyncer retains the real snap/2 peer-facing methods while
// observing only the actual state-sync launch boundary. The old cycle waits
// for cancellation; the new cycle reads the canonical pivot synchronously on
// its goroutine before allowing the downloader's queue to complete.
type pivotAuditSyncer struct {
	snap.Syncer
	oldHash   common.Hash
	oldNumber uint64
	d         *Downloader
	seen      chan common.Hash
	started   chan struct{}
}

func (s *pivotAuditSyncer) Sync(pivot *types.Header, cancel chan struct{}) error {
	if pivot.Hash() == s.oldHash {
		<-cancel
		return snap.ErrCancelled
	}
	got := rawdb.ReadCanonicalHash(s.d.stateDB, s.oldNumber)
	select {
	case s.seen <- got:
	default:
	}
	close(s.started)
	s.d.committed.Store(true)
	s.d.queue.Close()
	<-cancel
	return snap.ErrCancelled
}

// pivotReceiptDelayChain emulates a slow receipt-chain insertion. On the
// vulnerable ordering a new state sync is already scheduled before insertion,
// so it can observe the absent old-pivot index while this write is delayed.
// The fixed ordering must complete the insert before the retarget can start.
type pivotReceiptDelayChain struct {
	BlockChain
	retargetStarted <-chan struct{}
}

func (c *pivotReceiptDelayChain) InsertReceiptChain(
	blocks types.Blocks, receipts []rlp.RawValue, ancientLimit uint64,
) (int, error) {
	select {
	case <-c.retargetStarted:
	case <-time.After(20 * time.Millisecond):
	}
	return c.BlockChain.InsertReceiptChain(blocks, receipts, ancientLimit)
}

// TestSnapPivotRetargetNewStateSyncSeesCanonical exercises the whole
// processSnapSyncContent -> stateFetcher -> syncState goroutine handoff.
// It supplies a completed receipt queue and moves the pivot in the downloader
// test hook; the replacement state sync must see the old pivot's canonical
// hash after InsertReceiptChain, not an unindexed pivot.
func TestSnapPivotRetargetNewStateSyncSeesCanonical(t *testing.T) {
	genesis := &core.Genesis{Config: params.TestChainConfig}
	engine := beacon.New(ethash.NewFaker())
	_, blocks, receipts := core.GenerateChainWithGenesis(genesis, engine, 3, nil)
	tester := newTesterWithGenesis(t, SnapSync, nil, true, genesis, engine)
	defer tester.terminate()
	d := tester.downloader
	d.ancientLimit = 0
	previous := blocks[0].Header()
	target := blocks[2].Header()
	target.Root = common.HexToHash("0x01") // Force a retarget even with empty-body blocks.
	d.pivotHeader = previous

	audit := &pivotAuditSyncer{
		Syncer:  d.snapSyncer,
		oldHash: previous.Hash(), oldNumber: previous.Number.Uint64(),
		d: d, seen: make(chan common.Hash, 1), started: make(chan struct{}),
	}
	d.snapSyncer = audit
	d.blockchain = &pivotReceiptDelayChain{BlockChain: d.blockchain, retargetStarted: audit.started}

	// Feed a contiguous, fully retrieved segment ending before the new pivot.
	// The receipt-chain importer must commit both blocks before retarget.
	d.queue.Prepare(1, SnapSync)
	for i := 0; i < 2; i++ {
		block := blocks[i]
		result := newFetchResult(block.Header(), true, false)
		result.Transactions = block.Transactions()
		result.Uncles = block.Uncles()
		result.Withdrawals = block.Withdrawals()
		var err error
		result.Receipts, err = rlp.EncodeToBytes(receipts[i])
		if err != nil {
			t.Fatal(err)
		}
		result.pending.Store(0)
		d.queue.resultCache.items[i] = result
	}

	// Provide a valid skeleton status so movePivotIfStale can inspect bounds
	// without spuriously moving the explicitly selected new target.
	rawdb.WriteSkeletonHeader(tester.db, target)
	status, err := json.Marshal(&skeletonProgress{
		Subchains: []*subchain{{Head: target.Number.Uint64(), Tail: target.Number.Uint64(), Next: target.ParentHash}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawdb.WriteSkeletonSyncStatus(tester.db, status)
	d.chainInsertHook = func(results []*fetchResult) { d.pivotHeader = target }

	completed := make(chan error, 1)
	go func() { completed <- d.processSnapSyncContent() }()
	select {
	case got := <-audit.seen:
		if got != previous.Hash() {
			t.Errorf("new snap/2 sync saw canonical old pivot %v, want %v", got, previous.Hash())
		}
	case <-time.After(8 * time.Second):
		d.queue.Close()
		t.Fatal("replacement state sync was not started")
	}
	select {
	case err := <-completed:
		if err != nil && !errors.Is(err, snap.ErrCancelled) {
			t.Fatalf("downloader unexpectedly terminated: %v", err)
		}
	case <-time.After(8 * time.Second):
		d.queue.Close()
		t.Fatal("downloader did not finish after retarget observation")
	}
}
