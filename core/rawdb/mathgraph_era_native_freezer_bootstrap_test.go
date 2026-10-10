// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph test-only experiment: the EXISTING go-ethereum
// rawdb.InitDatabaseFromFreezer consumes a publisher-checksummed virtual
// genesis prefix without materializing block bodies or receipts.
// It also exposes a restart-safety residual when the ERA source disappears.
package rawdb_test

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

func TestMathGraphEraNativeFreezerIndexBootstrap(t *testing.T) {
	sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	backing := mathgraphFreshEraDatabase(t)
	view, err := mathgraphOpenVerifiedGenesisPrefix(backing, sources)
	if err != nil {
		t.Fatal(err)
	}

	if got := rawdb.ReadHeadHeaderHash(backing); got != (common.Hash{}) {
		t.Fatal("fresh backing database already has a head")
	}
	// Invoke upstream geth's real existing initialization path unchanged.
	rawdb.InitDatabaseFromFreezer(view)

	tip := sources[len(sources)-1].block.Hash()
	if got := rawdb.ReadHeadHeaderHash(backing); got != tip {
		t.Fatalf("native freezer header head = %s, expected %s", got, tip)
	}
	if got := rawdb.ReadHeadFastBlockHash(backing); got != tip {
		t.Fatalf("native freezer fast head = %s, expected %s", got, tip)
	}
	for i, src := range sources {
		n := uint64(i)
		hash := src.block.Hash()
		if number, ok := rawdb.ReadHeaderNumber(backing, hash); !ok || number != n {
			t.Fatalf("native freezer failed to populate hash->number index at %d", n)
		}
		if got := rawdb.ReadCanonicalHash(view, n); got != hash {
			t.Fatalf("virtual canonical block disappeared after native index bootstrap at %d", n)
		}
		if got := rawdb.ReadCanonicalBodyRLP(view, n, nil); !bytes.Equal(got, src.body) {
			t.Fatalf("virtual block body unavailable after native index bootstrap at %d", n)
		}
		// Only hash->number metadata is persisted by InitDatabaseFromFreezer.
		if got := rawdb.ReadCanonicalHash(backing, n); got != (common.Hash{}) {
			t.Fatalf("native bootstrap materialized canonical body index at %d", n)
		}
		if got := rawdb.ReadCanonicalBodyRLP(backing, n, nil); len(got) != 0 {
			t.Fatalf("native bootstrap imported block body at %d", n)
		}
		if got := rawdb.ReadCanonicalReceiptsRLP(backing, n, nil); len(got) != 0 {
			t.Fatalf("native bootstrap imported block receipts at %d", n)
		}
	}
	if frozen, err := backing.Ancients(); err != nil || frozen != 0 {
		t.Fatalf("native index init wrote backed ancient history: %d, err=%v", frozen, err)
	}
	if head := rawdb.ReadHeadBlockHash(backing); head != (common.Hash{}) {
		t.Fatalf("unexpected execution block head: %s", head)
	}
	t.Logf("VERIFIED_NATIVE_ERA_PREFIX_INDEX_BOOTSTRAP blocks=%d head=%s archive_body_import=0", len(sources), tip)
}

func TestMathGraphEraFreezerBootstrapCannotOutliveUnverifiedSource(t *testing.T) {
	sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	backing := mathgraphFreshEraDatabase(t)
	view, err := mathgraphOpenVerifiedGenesisPrefix(backing, sources)
	if err != nil {
		t.Fatal(err)
	}
	rawdb.InitDatabaseFromFreezer(view)

	// Simulate loss of the archive view: rawdb metadata now points to a head
	// whose header and canonical number->hash index are NOT in the local DB.
	// This is a documented obstruction, not a passing production restart.
	tip := sources[len(sources)-1].block.Hash()
	if rawdb.ReadHeadHeaderHash(backing) != tip {
		t.Fatal("the native index initialization did not execute")
	}
	if rawdb.ReadHeader(backing, tip, mathgraphGenesisPrefixLength-1) != nil {
		t.Fatal("unmounted archive unexpectedly retained the block header")
	}
	if rawdb.ReadCanonicalHash(backing, mathgraphGenesisPrefixLength-1) != (common.Hash{}) {
		t.Fatal("unmounted archive unexpectedly retained the canonical index")
	}
	// Reattaching an independently checksum-verified ERA view must restore
	// access to the exact same witness WITHOUT re-importing the 64 blocks.
	reattached, err := mathgraphOpenVerifiedGenesisPrefix(backing, sources)
	if err != nil {
		t.Fatalf("correct ERA archive cannot be reattached after index init: %v", err)
	}
	if got := rawdb.ReadHeader(reattached, tip, mathgraphGenesisPrefixLength-1); got == nil || got.Hash() != tip {
		t.Fatal("verified source reattachment did not restore indexed head")
	}
	if rawdb.ReadHeadBlockHash(backing) != (common.Hash{}) {
		t.Fatal("reattachment fabricated an execution state head")
	}
	t.Log("RESTART_RESIDUAL_ARCHIVE_BINDING_REQUIRED native metadata must be bound to verified ERA availability")
}

func TestMathGraphEraInvalidPrefixNeverStartsNativeBootstrap(t *testing.T) {
	sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	bad := append([]mathgraphEraSource(nil), sources...)
	bad[34].body = append(bytes.Clone(bad[34].body), 0x00)
	backing := mathgraphFreshEraDatabase(t)
	view, err := mathgraphOpenVerifiedGenesisPrefix(backing, bad)
	if err == nil || view != nil {
		t.Fatal("corrupt ERA history was admitted before native bootstrap")
	}
	if rawdb.ReadHeadHeaderHash(backing) != (common.Hash{}) ||
		rawdb.ReadHeadFastBlockHash(backing) != (common.Hash{}) {
		t.Fatal("rejected source changed native head pointers")
	}
	for _, source := range sources {
		if _, ok := rawdb.ReadHeaderNumber(backing, source.block.Hash()); ok {
			t.Fatal("rejected source left a hash-to-number index behind")
		}
	}
	t.Log("REJECTED_NATIVE_BOOTSTRAP_WITH_UNWARRANTED_ERA_PREFIX")
}
