// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph research fixture for ethereum/go-ethereum#35354.
// Exercise the REAL core.NewBlockChain constructor on a publisher-checksummed
// virtual ERA genesis prefix, with test-only archive binding. This does not
// launch a node service, peer sync, or install the overlay in production.
package rawdb_test

import (
    "os"
    "path/filepath"
    "testing"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/consensus/beacon"
    "github.com/ethereum/go-ethereum/consensus/ethash"
    "github.com/ethereum/go-ethereum/core"
    "github.com/ethereum/go-ethereum/core/rawdb"
)

func mathgraphSepoliaStartupConfig() *core.BlockChainConfig {
    cfg := core.DefaultConfig()
    // Isolate the constructor and its data contracts. Don't start a snapshot
    // builder or transaction indexer as incidental background services.
    cfg.SnapshotLimit = 0
    cfg.TxLookupLimit = -1
    cfg.TrieNoAsyncFlush = true
    return cfg
}

func TestMathGraphEraRealCoreNewBlockChainStartup(t *testing.T) {
    sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
    genesis := core.DefaultSepoliaGenesisBlock()
    genesisHash := genesis.ToBlock().Hash()
    if genesisHash != mathgraphPublishedSepoliaGenesis ||
        genesisHash != sources[0].block.Hash() {
        t.Fatalf("official Sepolia genesis mismatch: specification=%s ERA=%s", genesisHash, sources[0].block.Hash())
    }
    backing := mathgraphFreshEraDatabase(t)
    view, err := mathgraphGuardedEraBootstrap(backing, sources, mathgraphEra0Path())
    if err != nil {
        t.Fatalf("qualified archive bootstrap rejected: %v", err)
    }
    if rawdb.ReadHeadBlockHash(backing) != (common.Hash{}) {
        t.Fatal("index-only bootstrap invented an execution head")
    }

    // Unlike the prior low-level tests, this executes go-ethereum's real
    // chain constructor, including genesis handling, HeaderChain, chain-state
    // restoration and the immutable-head/fast-head distinction.
    bc, err := core.NewBlockChain(view, genesis, beacon.New(ethash.NewFaker()), mathgraphSepoliaStartupConfig())
    if err != nil {
        t.Fatalf("REAL_CORE_STARTUP_RESIDUAL core.NewBlockChain: %v", err)
    }
    defer bc.Stop()

    header := bc.CurrentHeader()
    if header == nil {
        t.Fatal("constructor returned a chain without a header head")
    }
    if header.Number.Uint64() != mathgraphGenesisPrefixLength-1 ||
        header.Hash() != sources[len(sources)-1].block.Hash() {
        t.Fatalf("verified archive header head not preserved: number=%d hash=%s", header.Number.Uint64(), header.Hash())
    }
    block := bc.CurrentBlock()
    if block == nil {
        t.Fatal("no execution head available")
    }
    // Header availability does not confer executed state. The only state
    // permitted to be newly written on startup is the official genesis.
    if block.Number.Uint64() != 0 || block.Hash() != genesisHash {
        t.Fatalf("unexecuted ERA history promoted to execution head: number=%d hash=%s", block.Number.Uint64(), block.Hash())
    }
    if !bc.HasState(block.Root) {
        t.Fatal("official genesis execution state was not initialized")
    }

    // The history after genesis must remain virtual, even though
    // core.NewBlockChain can read every header and body through the overlay.
    for n := uint64(1); n < mathgraphGenesisPrefixLength; n++ {
        if bc.GetHeaderByNumber(n) == nil || bc.GetBlockByNumber(n) == nil {
            t.Fatalf("verified archive block %d inaccessible to core.BlockChain", n)
        }
        if got := rawdb.ReadCanonicalHash(backing, n); got != (common.Hash{}) {
            t.Fatalf("constructor persisted archive canonical hash %d: %s", n, got)
        }
        if got := rawdb.ReadCanonicalBodyRLP(backing, n, nil); len(got) > 0 {
            t.Fatalf("constructor imported archive body at %d", n)
        }
        if got := rawdb.ReadCanonicalReceiptsRLP(backing, n, nil); len(got) > 0 {
            t.Fatalf("constructor imported archive receipts at %d", n)
        }
        if head := bc.CurrentBlock(); head.Number.Uint64() != 0 {
            t.Fatalf("block %d incorrectly promoted as executed state", n)
        }
    }
    // Where the archive tip's root differs from genesis, its trie must
    // remain absent until the state transition is actually executed.
    tipRoot := sources[len(sources)-1].block.Root()
    if tipRoot != block.Root && bc.HasState(tipRoot) {
        t.Fatal("unexecuted archive-tip state root became available")
    }

    t.Logf("QUALIFIED_REAL_CORE_BLOCKCHAIN_STARTUP archive_headers=%d execution_head=%d header_head=%d genesis_state_available=%t no_bulk_archive_import=true",
        len(sources), block.Number.Uint64(), header.Number.Uint64(), bc.HasState(block.Root))
}

func TestMathGraphEraRealCoreStartupRequiresSourceBeforeConstructor(t *testing.T) {
    sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
    genesis := core.DefaultSepoliaGenesisBlock()
    backing := mathgraphFreshEraDatabase(t)

    missing := filepath.Join(t.TempDir(), "missing-sepolia-era1.era1")
    if _, err := mathgraphGuardedEraBootstrap(backing, sources, missing); err == nil {
        t.Fatal("unavailable publisher archive was accepted before construction")
    }
    if got := rawdb.ReadHeadHeaderHash(backing); got != (common.Hash{}) {
        t.Fatalf("unverified archive created a head pointer: %s", got)
    }
    if got := rawdb.ReadChainConfig(backing, genesis.ToBlock().Hash()); got != nil {
        t.Fatal("unverified archive initialized chain configuration")
    }
    if present, _ := backing.Has(mathgraphEraBindingKey); present {
        t.Fatal("unverified archive wrote a restart certificate")
    }

    // Even if a valid archive disappears AFTER index initialization, a new
    // startup cannot proceed through this guard. The native constructor
    // itself is not claimed to enforce the rule.
    if _, err := mathgraphGuardedEraBootstrap(backing, sources, mathgraphEra0Path()); err != nil {
        t.Fatal(err)
    }
    if _, err := mathgraphGuardedEraReattach(backing, sources, missing); err == nil {
        t.Fatal("detached ERA archive was accepted by restart guard")
    }
    if got := rawdb.ReadHeadBlockHash(backing); got != (common.Hash{}) {
        t.Fatal("pre-constructor metadata fabricated a full execution head")
    }
    t.Log("REJECTED_CORE_STARTUP_WITHOUT_PUBLISHER_VERIFIED_ARCHIVE before constructor")
}

func TestMathGraphEraRealCoreStartupGenesisConflictFailsClosed(t *testing.T) {
    sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
    backing := mathgraphFreshEraDatabase(t)
    if _, err := mathgraphGuardedEraBootstrap(backing, sources, mathgraphEra0Path()); err != nil {
        t.Fatal(err)
    }
    // A deliberately mismatched genesis specification must not be installed
    // simply because all archived reads agree with one another.
    view, err := mathgraphGuardedEraReattach(backing, sources, mathgraphEra0Path())
    if err != nil {
        t.Fatal(err)
    }
    wrong := core.DefaultGenesisBlock()
    if wrong.ToBlock().Hash() == mathgraphPublishedSepoliaGenesis {
        t.Fatal("negative-control genesis unexpectedly matches Sepolia")
    }
    bc, startupErr := core.NewBlockChain(view, wrong,
        beacon.New(ethash.NewFaker()), mathgraphSepoliaStartupConfig())
    if bc != nil {
        bc.Stop()
        t.Fatal("mismatched genesis was accepted by native constructor")
    }
    if startupErr == nil {
        t.Fatal("native constructor did not reject incorrect network genesis")
    }
    if rawdb.ReadChainConfig(backing, wrong.ToBlock().Hash()) != nil {
        t.Fatal("incorrect network configuration was written")
    }
    if rawdb.ReadHeadBlockHash(backing) != (common.Hash{}) {
        t.Fatal("negative genesis test fabricated an execution head")
    }
    t.Logf("REJECTED_REAL_CORE_BLOCKCHAIN_GENESIS_MISMATCH: %v", startupErr)
}
