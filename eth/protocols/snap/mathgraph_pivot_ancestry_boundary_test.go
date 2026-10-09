// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Independent MathGraph qualification fixture. The same tests are compiled
// against the exact unfixed upstream parent and the exact maintainer PR head.
// A tiny version-specific adapter calls each revision's reorg predicate;
// the protected behavioral expectations are identical in both revisions.
package snap

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

// mathgraphPivotReorged is supplied by the qualification workflow, separately
// for the old function and the new method. No production file is patched.
func mathgraphPivotReorged(db ethdb.Database, prev, next *types.Header) bool

func mgBoundaryHeader(number uint64, parent common.Hash, root byte) *types.Header {
	return &types.Header{
		Number:     new(big.Int).SetUint64(number),
		ParentHash: parent,
		Root:       common.BytesToHash([]byte{root}),
		Difficulty: new(big.Int),
	}
}

func mgBoundaryChain(prev *types.Header, count int) []*types.Header {
	headers := make([]*types.Header, count)
	parent := prev
	for i := range headers {
		headers[i] = mgBoundaryHeader(parent.Number.Uint64()+1, parent.Hash(), byte(i%240+2))
		parent = headers[i]
	}
	return headers
}

func mgBoundaryWriteSkeleton(db ethdb.Database, headers []*types.Header) {
	for _, h := range headers {
		rawdb.WriteSkeletonHeader(db, h)
	}
}

func mgBoundaryCheck(t *testing.T, db ethdb.Database, prev, next *types.Header, wantReorg bool) {
	t.Helper()
	got := mathgraphPivotReorged(db, prev, next)
	if got != wantReorg {
		t.Fatalf("independent ancestry boundary: reorg=%v, want %v (old=%d %s new=%d %s)",
			got, wantReorg, prev.Number.Uint64(), prev.Hash(), next.Number.Uint64(), next.Hash())
	}
}

func TestMathGraphPivotAncestryIndependent(t *testing.T) {
	t.Run("SkeletonOnlyMissingCanonical", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(100, common.Hash{}, 1)
		chain := mgBoundaryChain(prev, 5)
		mgBoundaryWriteSkeleton(db, chain)
		if got := rawdb.ReadCanonicalHash(db, prev.Number.Uint64()); got != (common.Hash{}) {
			t.Fatalf("invalid setup: old pivot already canonical %s", got)
		}
		mgBoundaryCheck(t, db, prev, chain[len(chain)-1], false)
	})

	t.Run("DelayedOldPivotAcross134Blocks", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(2, common.Hash{}, 1)
		chain := mgBoundaryChain(prev, 134)
		mgBoundaryWriteSkeleton(db, chain)
		// The downloader has not imported the old pivot or any gap blocks.
		mgBoundaryCheck(t, db, prev, chain[len(chain)-1], false)
	})

	t.Run("MixedSkeletonAndImportedGap", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(100, common.Hash{}, 1)
		chain := mgBoundaryChain(prev, 5)
		for _, h := range chain[:2] {
			rawdb.WriteHeader(db, h)
			rawdb.WriteCanonicalHash(db, h.Hash(), h.Number.Uint64())
		}
		mgBoundaryWriteSkeleton(db, chain[2:])
		mgBoundaryCheck(t, db, prev, chain[len(chain)-1], false)
	})

	t.Run("WrongSkeletonHeaderButCorrectImportedHeader", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(100, common.Hash{}, 1)
		chain := mgBoundaryChain(prev, 5)
		mgBoundaryWriteSkeleton(db, chain)
		// An unrelated skeleton header at #102 must not conceal the actual
		// hash-linked imported header at the same height.
		other := mgBoundaryHeader(102, common.Hash{0xee}, 42)
		rawdb.WriteSkeletonHeader(db, other)
		rawdb.WriteHeader(db, chain[1])
		mgBoundaryCheck(t, db, prev, chain[len(chain)-1], false)
	})

	t.Run("RealReorgWithStaleCanonicalOldPivot", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(100, common.Hash{}, 1)
		other := mgBoundaryHeader(100, common.Hash{}, 9)
		chain := mgBoundaryChain(other, 5)
		mgBoundaryWriteSkeleton(db, chain)
		// Even though the old pivot remains indexed, it is not an ancestor.
		rawdb.WriteHeader(db, prev)
		rawdb.WriteCanonicalHash(db, prev.Hash(), prev.Number.Uint64())
		mgBoundaryCheck(t, db, prev, chain[len(chain)-1], true)
	})

	t.Run("MissingGapHeaderFailsClosed", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(100, common.Hash{}, 1)
		chain := mgBoundaryChain(prev, 5)
		mgBoundaryWriteSkeleton(db, chain[:2])
		mgBoundaryWriteSkeleton(db, chain[3:])
		// A missing ancestry witness is UNKNOWN, not proof of a real
		// reorg. The current conservative reset policy is still required.
		mgBoundaryCheck(t, db, prev, chain[len(chain)-1], true)
	})

	t.Run("NonAdvancingTargetFailsClosed", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(100, common.Hash{}, 1)
		mgBoundaryCheck(t, db, prev, mgBoundaryHeader(99, common.Hash{}, 2), true)
		mgBoundaryCheck(t, db, prev, prev, true)
	})

	t.Run("IndexedOldPivotDoesNotMaskMissingAncestry", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		prev := mgBoundaryHeader(100, common.Hash{}, 1)
		next := mgBoundaryHeader(105, common.Hash{0xee}, 2)
		rawdb.WriteHeader(db, prev)
		rawdb.WriteCanonicalHash(db, prev.Hash(), prev.Number.Uint64())
		mgBoundaryCheck(t, db, prev, next, true)
	})
}
