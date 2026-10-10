// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Test-only persistence separator: run the ACTUAL core.NewBlockChain with a
// publisher-checksummed ERA prefix on persistent LevelDB, close everything,
// reopen the same backing database and inspect the archive-binding boundary.
// A negative result is an explicit residual, not a success claim for restart.
package rawdb_test

import (
    "path/filepath"
    "strings"
    "testing"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/consensus/beacon"
    "github.com/ethereum/go-ethereum/consensus/ethash"
    "github.com/ethereum/go-ethereum/core"
    "github.com/ethereum/go-ethereum/core/rawdb"
    "github.com/ethereum/go-ethereum/ethdb"
    "github.com/ethereum/go-ethereum/ethdb/leveldb"
)

func mathgraphOpenPersistentEraDB(t *testing.T, root, eraPath string) ethdb.Database {
    t.Helper()
    kv, err := leveldb.New(filepath.Join(root, "leveldb"), 32, 32, "", false)
    if err != nil {
        t.Fatal(err)
    }
    db, err := rawdb.Open(kv, rawdb.OpenOptions{
        Ancient: filepath.Join(root, "ancients"),
        Era: filepath.Dir(eraPath),
    })
    if err != nil {
        kv.Close()
        t.Fatal(err)
    }
    return db
}

func TestMathGraphERACoreRealDiskRestartIdentifiesCanonicalGenesisResidual(t *testing.T) {
    sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
    archive, err := filepath.Abs(mathgraphEra0Path())
    if err != nil { t.Fatal(err) }
    root := t.TempDir()
    db1 := mathgraphOpenPersistentEraDB(t, root, archive)

    view, err := mathgraphGuardedEraBootstrap(db1, sources, archive)
    if err != nil {
        db1.Close()
        t.Fatal(err)
    }
    chain, err := core.NewBlockChain(view, core.DefaultSepoliaGenesisBlock(),
        beacon.New(ethash.NewFaker()), mathgraphSepoliaStartupConfig())
    if err != nil {
        db1.Close()
        t.Fatalf("core.NewBlockChain failed before restart: %v", err)
    }
    if chain.CurrentBlock().Number.Uint64() != 0 || chain.CurrentHeader().Number.Uint64() != mathgraphGenesisPrefixLength-1 {
        chain.Stop()
        db1.Close()
        t.Fatal("unexpected execution or archive header head")
    }
    chain.Stop()

    // The constructor legitimately persists Sepolia's genesis body and state.
    // This is not an archive bulk-import.
    if got := rawdb.ReadCanonicalHash(db1, 0); got != mathgraphPublishedSepoliaGenesis {
        db1.Close()
        t.Fatalf("native genesis was not persisted: %s", got)
    }
    for i := uint64(1); i < mathgraphGenesisPrefixLength; i++ {
        if got := rawdb.ReadCanonicalHash(db1,i); got != (common.Hash{}) {
            db1.Close()
            t.Fatalf("unexpected bulk canonical import at height %d", i)
        }
    }
    if err := db1.Close(); err != nil {
        t.Fatalf("first database close: %v", err)
    }

    // Actual disk reopen—not a new wrapper over the same memorydb.
    db2 := mathgraphOpenPersistentEraDB(t, root, archive)
    defer db2.Close()
    if got := rawdb.ReadCanonicalHash(db2, 0); got != mathgraphPublishedSepoliaGenesis {
        t.Fatalf("persisted genesis missing after disk restart: %s", got)
    }
    if got := rawdb.ReadHeadHeaderHash(db2); got != sources[len(sources)-1].block.Hash() {
        t.Fatalf("persisted header head changed after disk restart: %s", got)
    }
    if got := rawdb.ReadHeadBlockHash(db2); got != mathgraphPublishedSepoliaGenesis {
        t.Fatalf("persisted execution head changed after disk restart: %s", got)
    }
    if len(rawdb.ReadCanonicalBodyRLP(db2, mathgraphGenesisPrefixLength-1,nil)) != 0 {
        t.Fatal("archive body was secretly persisted")
    }
    if rawdb.ReadCanonicalHash(db2,mathgraphGenesisPrefixLength-1) != (common.Hash{}) {
        t.Fatal("archive tip canonical hash was secretly persisted")
    }

    // The earlier guard expected an empty backing canonical map at EVERY
    // virtual height, so it cannot yet tolerate the legitimate genesis
    // written by Geth. Preserve the exact obstruction and do not suppress.
    reattached, err := mathgraphGuardedEraReattach(db2, sources, archive)
    if reattached != nil || err == nil {
        t.Fatal("expected old strict prototype to reject persisted genesis: adjust this test if fixed")
    }
    if !strings.Contains(err.Error(), "existing canonical block at 0") {
        t.Fatalf("unexpected restart boundary, not genesis coexistence: %v", err)
    }
    t.Logf("CONFIRMED_ERA_RESTART_GENESIS_COEXISTENCE_RESIDUAL archive_headers=%d persisted_genesis=true no_bulk_import=true error=%q",
        mathgraphGenesisPrefixLength, err)
}
