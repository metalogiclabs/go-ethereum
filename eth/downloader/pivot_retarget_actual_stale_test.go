// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
package downloader

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// This research-only separator asks whether the reordering suffices when
// pending contiguous results have not yet reached the previously selected
// pivot. An authentic forward pivot move may occur while bodies/receipts lag.
// The new snap/2 cycle must not interpret a missing canonical old-pivot index
// as a chain reorganization before those receipts have been imported.
func TestSnapPivotRetargetRealStaleHeadWaitsForOldPivot(t *testing.T) {
	// Reuse the real Amsterdam test genesis and generate an actual
	// hash-linked chain whose state changes at the new pivot.
	genesis, _ := makeBALChain(1)
	engine := beacon.New(ethash.NewFaker())
	signer := types.LatestSigner(genesis.Config)
	_, blocks, receipts := core.GenerateChainWithGenesis(genesis, engine, 200, func(i int, block *core.BlockGen) {
		block.SetParentBeaconRoot(common.Hash{})
		if i == 135 {
			tx, err := types.SignTx(
				types.NewTransaction(block.TxNonce(testAddress), common.Address{0x01}, big.NewInt(1000), params.TxGas, block.BaseFee(), nil),
				signer, testKey,
			)
			if err != nil {
				panic(err)
			}
			block.AddTx(tx)
		}
	})
	tester := newTesterWithGenesis(t, SnapSync, nil, true, genesis, engine)
	defer tester.terminate()
	d := tester.downloader
	d.ancientLimit = 0
	previous := blocks[1].Header() // Old pivot is #2, not yet received.
	target := blocks[135].Header() // Head #200 moves stale pivot to #136.
	if previous.Root == target.Root {
		t.Fatal("new pivot did not change the state root")
	}
	d.pivotHeader = previous

	audit := &pivotAuditSyncer{
		Syncer:  d.snapSyncer,
		oldHash: previous.Hash(), oldNumber: previous.Number.Uint64(),
		d: d, seen: make(chan common.Hash, 1), started: make(chan struct{}),
	}
	d.snapSyncer = audit
	d.blockchain = &pivotReceiptDelayChain{BlockChain: d.blockchain, retargetStarted: audit.started}

	// Only #1 is ready in the downloader's contiguous results. #2 (the old
	// pivot) is still being downloaded, so even a successful receipt commit
	// for #1 cannot populate the old pivot's canonical-hash entry.
	d.queue.Prepare(1, SnapSync)
	block := blocks[0]
	result := newFetchResult(block.Header(), true, false)
	result.Transactions = block.Transactions()
	result.Uncles = block.Uncles()
	result.Withdrawals = block.Withdrawals()
	var err error
	result.Receipts, err = rlp.EncodeToBytes(receipts[0])
	if err != nil {
		t.Fatal(err)
	}
	result.pending.Store(0)
	d.queue.resultCache.items[0] = result

	// The old pivot (#2) arrives later than the initial result (#1).
	// The fixed downloader should keep the first state-sync cycle active
	// and continue consuming receipt results until #2 is indexed.
	second := blocks[1]
	delayed := newFetchResult(second.Header(), true, false)
	delayed.Transactions = second.Transactions()
	delayed.Uncles = second.Uncles()
	delayed.Withdrawals = second.Withdrawals()
	delayed.Receipts, err = rlp.EncodeToBytes(receipts[1])
	if err != nil {
		t.Fatal(err)
	}
	delayed.pending.Store(0)
	ready := make(chan struct{}, 1)
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		select {
		case <-ready:
		case <-time.After(8 * time.Second):
			return
		}
		time.Sleep(60 * time.Millisecond)
		d.queue.resultCache.lock.Lock()
		d.queue.resultCache.items[0] = delayed
		d.queue.resultCache.lock.Unlock()
		d.queue.lock.Lock()
		d.queue.active.Signal()
		d.queue.lock.Unlock()
	}()
	defer func() {
		select {
		case <-delivered:
		case <-time.After(9 * time.Second):
			t.Error("delayed pivot result feeder did not exit")
		}
	}()

	// Exercise the real movePivotIfStale decision. With head #200 and old
	// pivot #2, the next pivot is head-fsMinFullBlocks = #136. These are
	// actual contiguous headers from the generated chain, not a fake root.
	head := blocks[199].Header()
	tail := blocks[134].Header()
	if head.Number.Uint64()-uint64(fsMinFullBlocks) != target.Number.Uint64() {
		t.Fatal("unexpected stale-pivot selection")
	}
	for _, block := range blocks[134:] {
		rawdb.WriteSkeletonHeader(tester.db, block.Header())
	}
	status, err := json.Marshal(&skeletonProgress{
		Subchains: []*subchain{{Head: head.Number.Uint64(), Tail: tail.Number.Uint64(), Next: tail.ParentHash}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rawdb.WriteSkeletonSyncStatus(tester.db, status)
	// No synthetic pivot-change hook: downloader.movePivotIfStale itself
	// will retarget from block #2 to block #136 after the first queue result.
	d.chainInsertHook = func(_ []*fetchResult) {
		select {
		case ready <- struct{}{}:
		default:
		}
	}

	done := make(chan error, 1)
	go func() { done <- d.processSnapSyncContent() }()
	select {
	case observed := <-audit.seen:
		if observed != previous.Hash() {
			t.Errorf("missing-old-pivot retarget prematurely saw canonical %v, want %v (old pivot was not yet downloaded)", observed, previous.Hash())
		}
	case <-time.After(8 * time.Second):
		d.queue.Close()
		t.Fatal("missing-pivot retarget was not observed")
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, errCanceled) && !errors.Is(err, errCancelStateFetch) {
			// A canceled snap cycle is an expected finalizer outcome.
			t.Logf("downloader finalizer returned %v", err)
		}
	case <-time.After(8 * time.Second):
		d.queue.Close()
		t.Fatal("downloader did not terminate")
	}
}
