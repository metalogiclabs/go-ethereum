// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only causal test. The production hooks in this branch are no-ops when
// unset, and this test does not communicate with public Ethereum peers.
package downloader

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
)

type balImportCausalRow struct {
	Height uint64 `json:"height"`
	AttachedAtNS int64 `json:"attached_at_ns"`
	ImportedAtNS int64 `json:"imported_at_ns"`
	ReadyAtImport bool `json:"ready_at_import"`
	Receipts int `json:"receipts"`
}

type balImportCausalResult struct {
	Mode string `json:"mode"`
	Blocks int `json:"blocks"`
	PeerBALRequests uint64 `json:"peer_bal_requests"`
	Attached int `json:"attached"`
	ImportedReady int `json:"imported_ready"`
	Executed int `json:"executed"`
	Reconstructed int `json:"reconstructed"`
	ReleasedAfterImport bool `json:"released_after_import"`
	IdenticalHead bool `json:"identical_head"`
	IdenticalStateRoot bool `json:"identical_state_root"`
	Balance uint64 `json:"balance"`
	Rows []balImportCausalRow `json:"rows"`
}

type balTimeline struct {
	sync.Mutex
	attached map[uint64]int64
	imported map[uint64]int64
	ready map[uint64]bool
	duplicates int
}

func newBALCausalTester(t *testing.T, genesis *core.Genesis, success func()) *downloadTester {
	t.Helper()
	db, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := core.DefaultConfig()
	cfg.BALStateReconstruction = true
	bc, err := core.NewBlockChain(db, genesis, beacon.New(ethash.NewFaker()), cfg)
	if err != nil {
		t.Fatal(err)
	}
	tester := &downloadTester{
		db: db,
		chain: bc,
		peers: make(map[string]*downloadTesterPeer),
	}
	tester.downloader = New(db, ethconfig.FullSync, bc, tester.dropPeer, success, false)
	return tester
}

// runBALArrivalCase tests one actual Geth downloader against the same fully
// executed remote Ethereum chain. In early mode the existing test BAL gate
// withholds bodies until authenticated BAL attachment has completed. In late
// mode the same peer withholds BAL responses until all blocks are imported.
func runBALArrivalCase(t *testing.T, mode string, genesis *core.Genesis, blocks []*types.Block, serving *core.BlockChain) balImportCausalResult {
	t.Helper()
	success := make(chan struct{})
	tester := newBALCausalTester(t, genesis, func() { close(success) })
	defer tester.terminate()
	final := blocks[len(blocks)-1]
	tester.chain.SetFinalized(final.Header())

	start := time.Now()
	timeline := &balTimeline{
		attached: make(map[uint64]int64),
		imported: make(map[uint64]int64),
		ready: make(map[uint64]bool),
	}
	tester.downloader.queue.balAttachHook = func(n uint64) {
		timeline.Lock()
		if _, ok := timeline.attached[n]; ok {
			timeline.duplicates++
		}
		timeline.attached[n] = time.Since(start).Nanoseconds()
		timeline.Unlock()
	}
	tester.downloader.balImportHook = func(n uint64, available bool) {
		timeline.Lock()
		if _, ok := timeline.imported[n]; ok {
			timeline.duplicates++
		}
		timeline.imported[n] = time.Since(start).Nanoseconds()
		timeline.ready[n] = available
		timeline.Unlock()
	}

	var gate *balGate
	if mode == "ready-before-body" {
		hashes := make([]common.Hash, 0, len(blocks))
		for _, block := range blocks {
			hashes = append(hashes, block.Hash())
		}
		gate = newBALGate(hashes)
	}
	peer := tester.newPeerWithChain("causal-peer", eth.ETH71, serving, gate)
	var hold chan struct{}
	if mode == "delayed-until-import" {
		hold = make(chan struct{})
		peer.balDelayUntil = hold
	}
	if mode != "ready-before-body" && mode != "delayed-until-import" {
		t.Fatalf("unrecognized case %q", mode)
	}
	released := false
	defer func() {
		if hold != nil && !released {
			close(hold)
		}
	}()

	if err := tester.downloader.BeaconSync(final.Header(), final.Header()); err != nil {
		t.Fatalf("%s: BeaconSync: %v", mode, err)
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	headImported := false
	for !headImported {
		select {
		case <-tick.C:
			headImported = tester.chain.CurrentBlock().Hash() == final.Hash()
		case <-deadline.C:
			t.Fatalf("%s: no head import before deadline", mode)
		}
	}
	if hold != nil {
		if peer.balRequests.Load() == 0 {
			t.Fatalf("delayed case never requested a BAL")
		}
		// Release only AFTER all blocks reached the final head. Any BAL now
		// processed is too late to authorize execution avoidance.
		close(hold)
		released = true
	}
	select {
	case <-success:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: downloader did not complete after controlled release", mode)
	}

	head := tester.chain.CurrentBlock()
	if head.Hash() != final.Hash() || head.Root != final.Root() {
		t.Fatalf("%s: changed block hash or state root", mode)
	}
	state, err := tester.chain.State()
	if err != nil {
		t.Fatal(err)
	}
	balance := state.GetBalance(common.Address{0x01}).Uint64()
	if balance != uint64(len(blocks))*1000 {
		t.Fatalf("%s: recipient balance=%d, want %d", mode, balance, uint64(len(blocks))*1000)
	}
	result := balImportCausalResult{
		Mode: mode, Blocks: len(blocks), PeerBALRequests: peer.balRequests.Load(),
		ReleasedAfterImport: hold != nil, IdenticalHead: true, IdenticalStateRoot: true,
		Balance: balance,
	}
	timeline.Lock()
	if timeline.duplicates != 0 {
		timeline.Unlock()
		t.Fatalf("%s: %d duplicate event records", mode, timeline.duplicates)
	}
	for _, b := range blocks {
		n := b.NumberU64()
		importedAt, exists := timeline.imported[n]
		if !exists {
			timeline.Unlock()
			t.Fatalf("%s: missing actual import-load event at height %d", mode, n)
		}
		attachedAt, attached := timeline.attached[n]
		ready := timeline.ready[n]
		if ready && !attached {
			timeline.Unlock()
			t.Fatalf("%s: unproven BAL readiness at height %d", mode, n)
		}
		if ready && attachedAt > importedAt {
			timeline.Unlock()
			t.Fatalf("%s: BAL attachment occurred after import load at height %d", mode, n)
		}
		if attached {
			result.Attached++
		}
		if ready {
			result.ImportedReady++
		}
		receipts := len(tester.chain.GetReceiptsByHash(b.Hash()))
		switch receipts {
		case 0:
			result.Reconstructed++
		case 1:
			result.Executed++
		default:
			timeline.Unlock()
			t.Fatalf("%s: block %d had %d receipts instead of 0 or 1", mode, n, receipts)
		}
		if !ready && receipts != 1 {
			timeline.Unlock()
			t.Fatalf("%s: block %d skipped execution without an attached BAL", mode, n)
		}
		if ready && receipts != 0 {
			timeline.Unlock()
			t.Fatalf("%s: finality-eligible block %d with authenticated BAL still executed", mode, n)
		}
		if !attached {
			attachedAt = -1
		}
		result.Rows = append(result.Rows, balImportCausalRow{
			Height: n, AttachedAtNS: attachedAt, ImportedAtNS: importedAt,
			ReadyAtImport: ready, Receipts: receipts,
		})
	}
	timeline.Unlock()
	if result.PeerBALRequests == 0 || result.Executed+result.Reconstructed != len(blocks) {
		t.Fatalf("%s: missing BAL request or incomplete import: %+v", mode, result)
	}
	if mode == "ready-before-body" && (result.ImportedReady != len(blocks) || result.Reconstructed != len(blocks)) {
		t.Fatalf("ready-first body gate did not deliver 100%% authenticated BAL before import: %+v", result)
	}
	if mode == "delayed-until-import" && (result.ImportedReady != 0 || result.Reconstructed != 0) {
		t.Fatalf("delayed BAL responses bypassed normal execution: %+v", result)
	}
	return result
}

// Causal intervention: same immutable source data and finality assertion;
// mutate ONLY test-peer response ordering. The two arms discriminate whether
// importer-time authenticated BAL presence governs execution substitution.
func TestBALImportTimeAvailabilityCausalSeparator(t *testing.T) {
	genesis, blocks := makeBALChain(96)
	serving, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), genesis, beacon.New(ethash.NewFaker()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serving.Stop()
	if n, err := serving.InsertChain(blocks); n != len(blocks) || err != nil {
		t.Fatalf("building identical serving chain: %d/%d, %v", n, len(blocks), err)
	}

	var outcomes []balImportCausalResult
	for _, mode := range []string{"ready-before-body", "delayed-until-import"} {
		res := runBALArrivalCase(t, mode, genesis, blocks, serving)
		outcomes = append(outcomes, res)
		summary := res
		summary.Rows = nil
		raw, err := json.Marshal(summary)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("MG_BAL_CAUSAL_SUMMARY %s", raw)
		detail, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("MG_BAL_CAUSAL_ROWS %s", detail)
	}
	if outcomes[0].Balance != outcomes[1].Balance || !outcomes[0].IdenticalHead || !outcomes[1].IdenticalHead {
		t.Fatal("intervention did not preserve the protected state")
	}
	t.Logf("MG_BAL_CAUSAL_VERDICT %s", fmt.Sprintf("ready_first=%d/%d, delayed=%d/%d, state=%d", outcomes[0].Reconstructed, len(blocks), outcomes[1].Reconstructed, len(blocks), outcomes[0].Balance))
}
