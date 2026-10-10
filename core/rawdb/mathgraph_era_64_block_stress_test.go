// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Bounded 64-block extension of the independently qualified two/three-block
// research fixture. This is not a proof of chain finality or an archive syncer.
package rawdb_test

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
)

const mathgraphStressLength = 64

func mathgraph64EraSources(t *testing.T) []mathgraphEraSource {
	t.Helper()
	numbers := make([]uint64, mathgraphStressLength)
	for i := range numbers {
		numbers[i] = mathgraphEraBlockNumber - mathgraphStressLength + 1 + uint64(i)
	}
	return mathgraphLoadEraSources(t, numbers...)
}

func TestMathGraphEra64BlockVerifiedRange(t *testing.T) {
	source := mathgraph64EraSources(t)
	db := mathgraphFreshEraDatabase(t)
	view, err := mathgraphOpenAnchoredEraSegment(db, source, mathgraphPinnedWitness)
	if err != nil {
		t.Fatal(err)
	}
	first := mathgraphEraBlockNumber - mathgraphStressLength + 1
	encoded := rawdb.ReadHeaderRange(view, mathgraphEraBlockNumber, mathgraphStressLength)
	if len(encoded) != mathgraphStressLength {
		t.Fatalf("requested %d backwards headers, got %d", mathgraphStressLength, len(encoded))
	}
	for i, src := range source {
		n := first + uint64(i)
		hash := src.block.Hash()
		if crypto.Keccak256Hash(encoded[mathgraphStressLength-1-i]) != hash {
			t.Fatalf("indexed header-range hash mismatch at %d", n)
		}
		if rawdb.ReadCanonicalHash(view, n) != hash {
			t.Fatalf("canonical hash missing from verified span at %d", n)
		}
		if got := rawdb.ReadCanonicalBodyRLP(view, n, nil); !bytes.Equal(got, src.body) {
			t.Fatalf("body unavailable at %d", n)
		}
		if got := rawdb.ReadCanonicalReceiptsRLP(view, n, nil); !bytes.Equal(got, src.receipts) {
			t.Fatalf("receipts unavailable at %d", n)
		}
		if rawdb.ReadCanonicalHash(db, n) != (common.Hash{}) {
			t.Fatalf("one of %d virtual canonical entries was persisted at %d", mathgraphStressLength, n)
		}
		if i > 0 && src.block.ParentHash() != source[i-1].block.Hash() {
			t.Fatalf("unlinked verified predecessor at %d", n)
		}
	}
	if heads, err := view.AncientRange(rawdb.ChainFreezerHeaderTable, first, mathgraphStressLength, 0); err != nil || len(heads) != mathgraphStressLength {
		t.Fatalf("64-header range unavailable: count=%d err=%v", len(heads), err)
	}
	if _, err := view.AncientRange(rawdb.ChainFreezerBodiesTable, first-1, mathgraphStressLength+1, 0); err == nil {
		t.Fatal("unverified prehistory was silently joined to the certified range")
	}
	if frozen, err := db.Ancients(); err != nil || frozen != 0 {
		t.Fatalf("invented canonical ancient prefix: frozen=%d err=%v", frozen, err)
	}
	t.Logf("VERIFIED_64_BLOCK_ERA_SEGMENT first=%d last=%d tip=%s backing_ancients=0", first, mathgraphEraBlockNumber, source[len(source)-1].block.Hash())
}

func TestMathGraphEra64BlockRejectsInteriorFailures(t *testing.T) {
	source := mathgraph64EraSources(t)
	first := mathgraphEraBlockNumber - mathgraphStressLength + 1
	tests := []struct {
		name   string
		mutate func([]mathgraphEraSource) []mathgraphEraSource
	}{
		{"MissingInteriorBlock", func(s []mathgraphEraSource) []mathgraphEraSource {
			return append(s[:25:25], s[26:]...)
		}},
		{"CorruptInteriorBody", func(s []mathgraphEraSource) []mathgraphEraSource {
			s[31].body = append(bytes.Clone(s[31].body), 0)
			return s
		}},
		{"CorruptInteriorReceipts", func(s []mathgraphEraSource) []mathgraphEraSource {
			s[32].receipts = append(bytes.Clone(s[32].receipts), 0)
			return s
		}},
		{"SwappedInteriorBlocks", func(s []mathgraphEraSource) []mathgraphEraSource {
			s[42], s[43] = s[43], s[42]
			return s
		}},
		{"DuplicateInteriorBlock", func(s []mathgraphEraSource) []mathgraphEraSource {
			s[20] = s[19]
			return s
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := mathgraphFreshEraDatabase(t)
			testSources := append([]mathgraphEraSource(nil), source...)
			testSources = tc.mutate(testSources)
			v, err := mathgraphOpenAnchoredEraSegment(db, testSources, mathgraphPinnedWitness)
			if err == nil || v != nil {
				t.Fatal("uncertified interior history became visible")
			}
			for _, n := range []uint64{first, first+31, mathgraphEraBlockNumber} {
				if got := rawdb.ReadCanonicalHash(db, n); got != (common.Hash{}) {
					t.Fatalf("rejected interior history wrote index at %d", n)
				}
			}
		})
	}
	t.Run("PreexistingCanonicalAtInterior", func(t *testing.T) {
		db := mathgraphFreshEraDatabase(t)
		n := first + 33
		other := common.Hash{0xba}
		rawdb.WriteCanonicalHash(db, other, n)
		v, err := mathgraphOpenAnchoredEraSegment(db, source, mathgraphPinnedWitness)
		if err == nil || v != nil {
			t.Fatal("interior canonical conflict concealed")
		}
		if got := rawdb.ReadCanonicalHash(db, n); got != other {
			t.Fatal("rejecting conflicting history modified backing database")
		}
	})
	t.Run("BeyondExplicit64BlockBound", func(t *testing.T) {
		db := mathgraphFreshEraDatabase(t)
		extra := mathgraphLoadEraSources(t, first-1)
		s := append(extra, source...)
		v, err := mathgraphOpenAnchoredEraSegment(db, s, mathgraphPinnedWitness)
		if err == nil || v != nil {
			t.Fatal("range past declared bound was admitted")
		}
	})
	t.Log("REJECTED_64_BLOCK_INTERIOR_GAP_FORK_CORRUPTION_AND_CONFLICT")
}

func TestMathGraphEra64BlockConcurrentReadOnly(t *testing.T) {
	source := mathgraph64EraSources(t)
	db := mathgraphFreshEraDatabase(t)
	view, err := mathgraphOpenAnchoredEraSegment(db, source, mathgraphPinnedWitness)
	if err != nil {
		t.Fatal(err)
	}
	first := mathgraphEraBlockNumber - mathgraphStressLength + 1
	const workers, repetitions = 8, 24
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < repetitions; i++ {
				idx := (worker*19 + i*23) % mathgraphStressLength
				n := first + uint64(idx)
				if got := rawdb.ReadCanonicalHash(view, n); got != source[idx].block.Hash() {
					errs <- fmt.Errorf("worker %d canonical mismatch at %d", worker, n)
					return
				}
				if got := rawdb.ReadCanonicalBodyRLP(view, n, nil); !bytes.Equal(got, source[idx].body) {
					errs <- fmt.Errorf("worker %d body mismatch at %d", worker, n)
					return
				}
				if rawdb.ReadCanonicalHash(db, n) != (common.Hash{}) {
					errs <- fmt.Errorf("worker %d observed backing mutation at %d", worker, n)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	t.Logf("VERIFIED_64_BLOCK_CONCURRENT_READ_ONLY workers=%d reads_per_worker=%d", workers, repetitions)
}
