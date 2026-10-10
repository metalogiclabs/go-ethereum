// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only experiment. Grace waiting is zero by default, and no upstream
// node, public peer or consensus policy is changed by this test.
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
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
)

type balGraceCase struct {
	name     string
	protocol uint
	window   time.Duration
	reply    string // ready-plus-2ms, after-import, or immediate
	bodyGate bool
}

type balGraceOutcome struct {
	Case                  string  `json:"case"`
	Protocol              uint    `json:"protocol"`
	GraceBudgetMS         int64   `json:"grace_budget_ms"`
	Head                  string  `json:"head"`
	StateRoot             string  `json:"state_root"`
	SenderNonce           uint64  `json:"sender_nonce"`
	Blocks                int     `json:"blocks"`
	Reconstructed         int     `json:"reconstructed"`
	Executed              int     `json:"executed"`
	ReadyAtImport         int     `json:"ready_at_import"`
	Attached              int     `json:"attached"`
	BALRequests           uint64  `json:"bal_requests"`
	WaitCalls             int     `json:"wait_calls"`
	ExpiredWaitCalls      int     `json:"expired_wait_calls"`
	WaitDurationMS        float64 `json:"wait_duration_ms"`
	MaxOneWaitMS          float64 `json:"max_one_wait_ms"`
	ChargedBudgetMS       float64 `json:"charged_budget_ms"`
	PeerReleasedAfterHead bool    `json:"peer_released_after_head"`
	HeadBeforeRelease     bool    `json:"head_before_release"`
}

type balGraceEvents struct {
	sync.Mutex
	imported         map[uint64]bool
	attached         map[uint64]bool
	waitCalls        int
	expired          int
	total            time.Duration
	maxWait          time.Duration
	duplicateImports int
}

// runBALGraceArm runs the real downloader with the unchanged source chain.
// The timing intervention is anchored to a COMMON mandatory-data-ready event:
// both the immediate arm and grace-enabled arm receive the same BAL reply
// precisely two milliseconds after that event.
func runBALGraceArm(t *testing.T, cfg balGraceCase, genesis *core.Genesis, serving *core.BlockChain, blocks []*types.Block, expectedNonce uint64) balGraceOutcome {
	t.Helper()
	success := make(chan struct{})
	tester := newBALCausalTester(t, genesis, func() { close(success) })
	defer tester.terminate()
	final := blocks[len(blocks)-1]
	tester.chain.SetFinalized(final.Header())

	events := &balGraceEvents{
		imported: make(map[uint64]bool),
		attached: make(map[uint64]bool),
	}
	ready := make(chan struct{}, 1)
	var readyOnce sync.Once
	q := tester.downloader.queue
	q.balGraceWindow = cfg.window
	q.balGraceRequireInFlight = true
	q.balBatchReadyHook = func() {
		readyOnce.Do(func() { ready <- struct{}{} })
	}
	q.balGraceStartHook = func(missing int) {
		if missing <= 0 {
			t.Errorf("%s: grace invoked without missing eligible BAL", cfg.name)
		}
		events.Lock()
		events.waitCalls++
		events.Unlock()
	}
	q.balGraceEndHook = func(d time.Duration, expired bool) {
		events.Lock()
		events.total += d
		if d > events.maxWait {
			events.maxWait = d
		}
		if expired {
			events.expired++
		}
		events.Unlock()
	}
	q.balAttachHook = func(n uint64) {
		events.Lock()
		events.attached[n] = true
		events.Unlock()
	}
	tester.downloader.balImportHook = func(n uint64, hadBAL bool) {
		events.Lock()
		if _, exists := events.imported[n]; exists {
			events.duplicateImports++
		}
		events.imported[n] = hadBAL
		events.Unlock()
	}

	var gate *balGate
	if cfg.bodyGate {
		hashes := make([]common.Hash, 0, len(blocks))
		for _, b := range blocks {
			hashes = append(hashes, b.Hash())
		}
		gate = newBALGate(hashes)
	}
	peer := tester.newPeerWithChain("grace-peer", cfg.protocol, serving, gate)
	var withheld chan struct{}
	if cfg.reply == "ready-plus-2ms" || cfg.reply == "after-import" {
		withheld = make(chan struct{})
		peer.balDelayUntil = withheld
	}
	var releaseOnce sync.Once
	release := func() {
		if withheld != nil {
			releaseOnce.Do(func() { close(withheld) })
		}
	}
	defer release()
	done := make(chan struct{})
	defer close(done)
	if cfg.reply == "ready-plus-2ms" {
		go func() {
			select {
			case <-ready:
				timer := time.NewTimer(2 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					release()
				case <-done:
				}
			case <-done:
			}
		}()
	}
	if cfg.reply != "ready-plus-2ms" && cfg.reply != "after-import" && cfg.reply != "immediate" {
		t.Fatalf("bad response schedule: %s", cfg.reply)
	}
	if err := tester.downloader.BeaconSync(final.Header(), final.Header()); err != nil {
		t.Fatalf("%s BeaconSync: %v", cfg.name, err)
	}

	// The head must reach the target before any late-held response is released,
	// proving non-blocking import under the declared finite grace policy.
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	complete := false
	for !complete {
		select {
		case <-tick.C:
			complete = tester.chain.CurrentBlock().Hash() == final.Hash()
		case <-deadline.C:
			t.Fatalf("%s: importer blocked beyond 12s without BAL", cfg.name)
		}
	}
	afterHead := cfg.reply == "after-import"
	if afterHead {
		if cfg.protocol >= eth.ETH71 && peer.balRequests.Load() == 0 {
			t.Fatalf("%s: no BAL was requested before import", cfg.name)
		}
		release()
	}
	select {
	case <-success:
	case <-time.After(12 * time.Second):
		t.Fatalf("%s: downloader did not finish following peer response release", cfg.name)
	}
	if h := tester.chain.CurrentBlock(); h.Hash() != final.Hash() || h.Root != final.Root() {
		t.Fatalf("%s: head/root mismatch", cfg.name)
	}
	state, err := tester.chain.State()
	if err != nil {
		t.Fatal(err)
	}
	nonce := state.GetNonce(testAddress)
	if nonce != expectedNonce {
		t.Fatalf("%s: nonce %d, independently executed nonce %d", cfg.name, nonce, expectedNonce)
	}
	result := balGraceOutcome{
		Case: cfg.name, Protocol: cfg.protocol, GraceBudgetMS: cfg.window.Milliseconds(),
		Head: final.Hash().Hex(), StateRoot: final.Root().Hex(), SenderNonce: nonce,
		Blocks: len(blocks), BALRequests: peer.balRequests.Load(),
		PeerReleasedAfterHead: afterHead, HeadBeforeRelease: afterHead,
	}
	events.Lock()
	for _, b := range blocks {
		n := b.NumberU64()
		readyAtImport, seen := events.imported[n]
		if !seen {
			events.Unlock()
			t.Fatalf("%s: missing import event at %d", cfg.name, n)
		}
		attached := events.attached[n]
		if readyAtImport && !attached {
			events.Unlock()
			t.Fatalf("%s: no validated attachment for ready block %d", cfg.name, n)
		}
		if attached {
			result.Attached++
		}
		if readyAtImport {
			result.ReadyAtImport++
		}
		receipts := len(tester.chain.GetReceiptsByHash(b.Hash()))
		switch receipts {
		case 0:
			result.Reconstructed++
		case 1:
			result.Executed++
		default:
			events.Unlock()
			t.Fatalf("%s: invalid receipt count at %d: %d", cfg.name, n, receipts)
		}
		if (receipts == 0) != readyAtImport {
			events.Unlock()
			t.Fatalf("%s: importer BAL decision differs from execution at block %d", cfg.name, n)
		}
	}
	result.WaitCalls = events.waitCalls
	result.ExpiredWaitCalls = events.expired
	result.WaitDurationMS = float64(events.total) / float64(time.Millisecond)
	result.MaxOneWaitMS = float64(events.maxWait) / float64(time.Millisecond)
	result.ChargedBudgetMS = float64(q.balGraceSpent) / float64(time.Millisecond)
	dup := events.duplicateImports
	events.Unlock()
	if dup != 0 || result.Executed+result.Reconstructed != len(blocks) {
		t.Fatalf("%s: duplicate or missing import event", cfg.name)
	}
	// Hard policy cap is 25ms, with generous allowance for CI preemption.
	// A serious wait overrun cannot be promoted as a viable bounded policy.
	if result.ChargedBudgetMS > 25.00001 {
		t.Fatalf("%s: global BAL waiting budget overrun: %+v", cfg.name, result)
	}
	if result.MaxOneWaitMS > 1000 {
		t.Fatalf("%s: observed grace wait exceeded liveness guard: %+v", cfg.name, result)
	}
	return result
}

// The protected controls use identical finalized chain inputs. A BAL reply is
// either immediate, delayed a fixed 2ms after mandatory data is ready, or held
// through head import; legacy peers cannot serve any BAL.
func TestBALBoundedGraceProtectedPolicy(t *testing.T) {
	gspec, blocks := makeBALChain(96)
	serving, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), gspec, beacon.New(ethash.NewFaker()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serving.Stop()
	if n, err := serving.InsertChain(blocks); err != nil || n != len(blocks) {
		t.Fatalf("source chain: %d/%d: %v", n, len(blocks), err)
	}
	source, err := serving.State()
	if err != nil {
		t.Fatal(err)
	}
	expectedNonce := source.GetNonce(testAddress)
	if expectedNonce == 0 {
		t.Fatal("source fixture did not execute nontrivial transactions")
	}

	cases := []balGraceCase{
		{name: "immediate-delayed", protocol: eth.ETH71, window: 0, reply: "ready-plus-2ms"},
		{name: "grace-delayed", protocol: eth.ETH71, window: 20 * time.Millisecond, reply: "ready-plus-2ms"},
		{name: "grace-response-after-import", protocol: eth.ETH71, window: 5 * time.Millisecond, reply: "after-import"},
		{name: "grace-legacy-no-bal", protocol: eth.ETH69, window: 5 * time.Millisecond, reply: "immediate"},
		{name: "grace-early-bal", protocol: eth.ETH71, window: 20 * time.Millisecond, reply: "immediate", bodyGate: true},
	}
	outcomes := make(map[string]balGraceOutcome)
	for _, c := range cases {
		obs := runBALGraceArm(t, c, gspec, serving, blocks, expectedNonce)
		outcomes[c.name] = obs
		raw, err := json.Marshal(obs)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("MG_BAL_GRACE_ARM %s", raw)
	}
	immediate := outcomes["immediate-delayed"]
	grace := outcomes["grace-delayed"]
	late := outcomes["grace-response-after-import"]
	legacy := outcomes["grace-legacy-no-bal"]
	early := outcomes["grace-early-bal"]

	if grace.Reconstructed <= immediate.Reconstructed {
		t.Fatalf("bounded grace failed to improve matched delayed reply: immediate=%d grace=%d", immediate.Reconstructed, grace.Reconstructed)
	}
	if grace.Reconstructed == 0 || grace.WaitCalls == 0 {
		t.Fatalf("bounded grace did not acquire any verified BAL or start waiting: %+v", grace)
	}
	if late.Reconstructed != 0 || late.ExpiredWaitCalls == 0 || !late.HeadBeforeRelease {
		t.Fatalf("late peer must execute and time out before release: %+v", late)
	}
	if legacy.Reconstructed != 0 || legacy.BALRequests != 0 || legacy.Executed != len(blocks) || legacy.WaitCalls != 0 {
		t.Fatalf("legacy peer did not safely execute all blocks: %+v", legacy)
	}
	if early.Reconstructed != len(blocks) || early.ExpiredWaitCalls != 0 {
		t.Fatalf("already-available authenticated BALs did not take correct path: %+v", early)
	}
	t.Logf("MG_BAL_GRACE_VERDICT %s",
		fmt.Sprintf("immediate=%d/%d grace=%d/%d late=%d/%d legacy=%d/%d early=%d/%d; global-wait-budget<=25ms legacy-waits=%d", 
			immediate.Reconstructed, len(blocks), grace.Reconstructed, len(blocks),
			late.Reconstructed, len(blocks), legacy.Reconstructed, len(blocks), early.Reconstructed, len(blocks), legacy.WaitCalls))
}

// This isolated policy-law test proves the waiting budget is not renewed
// when more completed batches arrive. The synthetic queue has a ready block
// with its authenticated BAL still pending and one actual in-flight request.
func TestBALBoundedGraceGlobalBudgetCannotCompound(t *testing.T) {
	q := newQueue(4, 1)
	item := &fetchResult{Header: new(types.Header)}
	item.pending.Store(1 << balType)
	q.resultCache.items[0] = item
	q.balPendPool["in-flight"] = new(fetchRequest)
	q.balGraceWindow = 20 * time.Millisecond
	q.balGraceRequireInFlight = true
	starts := 0
	q.balGraceStartHook = func(int) { starts++ }
	start := time.Now()
	for i := 0; i < 4; i++ {
		q.waitForCompletedBALGrace()
	}
	elapsed := time.Since(start)
	if starts != 2 {
		t.Fatalf("expected 20ms + 5ms capped waits, got %d", starts)
	}
	if q.balGraceSpent != 25*time.Millisecond {
		t.Fatalf("cumulative budget=%v, want 25ms", q.balGraceSpent)
	}
	if elapsed > time.Second {
		t.Fatalf("bounded wait spent too much observed time: %v", elapsed)
	}
	t.Logf("MG_BAL_GRACE_GLOBAL_BUDGET windows=%d charged=%s elapsed=%s", starts, q.balGraceSpent, elapsed)
}
