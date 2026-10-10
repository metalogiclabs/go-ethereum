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

func mathgraphTryOpenPersistentEraDB(root, eraPath string) (ethdb.Database, error) {
    kv, err := leveldb.New(filepath.Join(root, "leveldb"), 32, 32, "", false)
    if err != nil {
        return nil, err
    }
    db, err := rawdb.Open(kv, rawdb.OpenOptions{
        Ancient: filepath.Join(root, "ancients"),
        Era: filepath.Dir(eraPath),
    })
    if err != nil {
        kv.Close()
        return nil, err
    }
    return db, nil
}

func mathgraphOpenPersistentEraDB(t *testing.T, root, eraPath string) ethdb.Database {
    t.Helper()
    db, err := mathgraphTryOpenPersistentEraDB(root, eraPath)
    if err != nil { t.Fatal(err) }
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

    // Real persistent disk reopen. Geth's first-party rawdb.Open rejects
    // the bootstrap BEFORE our higher-level archive certificate guard runs:
    // canonical genesis and head #63 exist in LevelDB, but the freezer is
    // empty, and its continuity guard expects block #1 in LevelDB.
    reopened, openErr := mathgraphTryOpenPersistentEraDB(root, archive)
    if reopened != nil || openErr == nil {
        if reopened != nil { reopened.Close() }
        t.Fatal("the current standard opener unexpectedly accepted an unmaterialized ERA prefix")
    }
    if !strings.Contains(openErr.Error(), "ancient chain segments already extracted") {
        t.Fatalf("unexpected database-opening obstacle: %v", openErr)
    }

    // Verify the original LevelDB storage survived the reopen rejection.
    // This is a diagnostic raw-KV read, NOT permission to bypass the normal
    // rawdb.Open guard in any actual Geth node.
    kv, err := leveldb.New(filepath.Join(root, "leveldb"), 32, 32, "", false)
    if err != nil { t.Fatal(err) }
    direct := rawdb.NewDatabase(kv)
    defer direct.Close()
    if got := rawdb.ReadCanonicalHash(direct, 0); got != mathgraphPublishedSepoliaGenesis {
        t.Fatalf("persisted genesis missing after disk restart: %s", got)
    }
    if got := rawdb.ReadHeadHeaderHash(direct); got != sources[len(sources)-1].block.Hash() {
        t.Fatalf("persisted header head changed after disk restart: %s", got)
    }
    if got := rawdb.ReadHeadBlockHash(direct); got != mathgraphPublishedSepoliaGenesis {
        t.Fatalf("persisted execution head changed after disk restart: %s", got)
    }
    if len(rawdb.ReadCanonicalBodyRLP(direct, mathgraphGenesisPrefixLength-1, nil)) != 0 {
        t.Fatal("archive body was secretly persisted")
    }
    if rawdb.ReadCanonicalHash(direct, mathgraphGenesisPrefixLength-1) != (common.Hash{}) {
        t.Fatal("archive tip canonical hash was secretly persisted")
    }
    if ok, err := direct.Has(mathgraphEraBindingKey); err != nil || !ok {
        t.Fatal("persisted archive provenance certificate missing")
    }
    if err := mathgraphCheckPublishedArchiveAt(archive); err != nil {
        t.Fatalf("verified ERA source changed: %v", err)
    }
    t.Logf("CONFIRMED_ERA_RAWDB_OPEN_CONTINUITY_RESIDUAL persisted_genesis=true indexed_tip=%d archive_sha_valid=true no_bulk_import=true error=%q",
        mathgraphGenesisPrefixLength-1, openErr)
}
