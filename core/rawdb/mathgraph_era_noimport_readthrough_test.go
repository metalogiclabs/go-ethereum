// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph *test-only*, read-only witness-backed ancient overlay.
// This tests whether one cryptographically constrained ERA block can satisfy
// ordinary rawdb canonical readers without importing it into the backing DB.
// It does NOT enable automatic archive sync or advertise a contiguous freezer.
package rawdb_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

// mathgraphReadOnlyEraView overlays one separately warranted block on the
// ordinary ancient read interface. No writes to the backing KV/freezer occur.
type mathgraphReadOnlyEraView struct {
	ethdb.Database
	number uint64
	data   map[string][]byte
}

func (v *mathgraphReadOnlyEraView) Ancient(kind string, number uint64) ([]byte, error) {
	if number == v.number {
		if blob, ok := v.data[kind]; ok {
			return bytes.Clone(blob), nil
		}
	}
	return v.Database.Ancient(kind, number)
}

func (v *mathgraphReadOnlyEraView) ReadAncients(fn func(ethdb.AncientReaderOp) error) error {
	// Preserve the underlying freezer read lock while redirecting its reader
	// to this verified, immutable one-block overlay.
	return v.Database.ReadAncients(func(_ ethdb.AncientReaderOp) error {
		return fn(v)
	})
}

func mathgraphOpenReadOnlyWitnessView(db ethdb.Database, source mathgraphEraSource, witness mathgraphEraWitness) (*mathgraphReadOnlyEraView, error) {
	// Explicitly reject an existing backing-chain claim before overlaying a
	// view, even if it happens to match. This is NOT a general node API.
	if existing := rawdb.ReadCanonicalHash(db, witness.number); existing != (common.Hash{}) {
		return nil, fmt.Errorf("backing canonical index already populated at %d: %s", witness.number, existing)
	}

	// Reuse the independently qualified validation logic from the isolated
	// materialization experiment. The scratch DB is discarded; the caller's
	// database is never populated, imported or modified.
	scratch := rawdb.NewMemoryDatabase()
	defer scratch.Close()
	if err := mathgraphMaterializeOneVerifiedEraBlock(scratch, source, witness); err != nil {
		return nil, fmt.Errorf("ERA boundary rejected: %w", err)
	}

	headerRLP := rawdb.ReadHeaderRLP(scratch, witness.headerHash, witness.number)
	bodyRLP := rawdb.ReadBodyRLP(scratch, witness.headerHash, witness.number)
	receiptsRLP := rawdb.ReadReceiptsRLP(scratch, witness.headerHash, witness.number)
	if len(headerRLP) == 0 || len(bodyRLP) == 0 || len(receiptsRLP) == 0 {
		return nil, fmt.Errorf("verified witness missing one or more components")
	}
	hashBytes := bytes.Clone(witness.headerHash[:])
	return &mathgraphReadOnlyEraView{
		Database: db,
		number:   witness.number,
		data: map[string][]byte{
			rawdb.ChainFreezerHashTable:    hashBytes,
			rawdb.ChainFreezerHeaderTable:  bytes.Clone(headerRLP),
			rawdb.ChainFreezerBodiesTable:  bytes.Clone(bodyRLP),
			rawdb.ChainFreezerReceiptTable: bytes.Clone(receiptsRLP),
		},
	}, nil
}

func TestMathGraphEraNoImportCanonicalReadThrough(t *testing.T) {
	source := mathgraphLoadEraSource(t)
	db := mathgraphFreshEraDatabase(t)
	w := mathgraphPinnedWitness

	// Prove this database has not indexed the ERA data.
	if h := rawdb.ReadCanonicalHash(db, w.number); h != (common.Hash{}) {
		t.Fatal("expected a fresh database without the block index")
	}
	if d := rawdb.ReadCanonicalBodyRLP(db, w.number, nil); len(d) > 0 {
		t.Fatal("expected body absent before read-only overlay")
	}

	view, err := mathgraphOpenReadOnlyWitnessView(db, source, w)
	if err != nil {
		t.Fatal(err)
	}
	if got := rawdb.ReadCanonicalHash(view, w.number); got != w.headerHash {
		t.Fatalf("overlay canonical hash: got %s, want %s", got, w.headerHash)
	}
	if header := rawdb.ReadHeader(view, w.headerHash, w.number); header == nil || header.Hash() != w.headerHash {
		t.Fatal("verified ERA header not accessible via normal rawdb reader")
	}
	if got := rawdb.ReadCanonicalBodyRLP(view, w.number, nil); !bytes.Equal(got, source.body) {
		t.Fatal("canonical body did not read through to ERA evidence")
	}
	if got := rawdb.ReadCanonicalReceiptsRLP(view, w.number, nil); !bytes.Equal(got, source.receipts) {
		t.Fatal("canonical receipts did not read through to ERA evidence")
	}
	if block := rawdb.ReadBlock(view, w.headerHash, w.number); block == nil || block.Hash() != w.headerHash {
		t.Fatal("normal ReadBlock failed through verified ERA view")
	}
	if got := rawdb.ReadBodyRLP(view, common.Hash{0xee}, w.number); len(got) != 0 {
		t.Fatal("incorrect block hash accessed a body through the verified view")
	}
	if got := rawdb.ReadHeaderRLP(view, common.Hash{0xee}, w.number); len(got) != 0 {
		t.Fatal("incorrect hash accessed a header through the verified view")
	}
	if got := rawdb.ReadCanonicalHash(view, w.number-1); got != (common.Hash{}) {
		t.Fatal("unverified previous block exposed by view")
	}
	if got := rawdb.ReadCanonicalHash(view, w.number+1); got != (common.Hash{}) {
		t.Fatal("unverified next block exposed by view")
	}

	// The backing database is still unmodified. No history import occurred.
	if got := rawdb.ReadCanonicalHash(db, w.number); got != (common.Hash{}) {
		t.Fatalf("read-only view wrote a canonical index: %s", got)
	}
	if got := rawdb.ReadCanonicalBodyRLP(db, w.number, nil); len(got) != 0 {
		t.Fatal("read-only view imported the block body")
	}
	if frozen, err := db.Ancients(); err != nil || frozen != 0 {
		t.Fatalf("read-only view invented a contiguous freezer: head=%d err=%v", frozen, err)
	}
	t.Logf("VERIFIED_ERA_NO_IMPORT_READ_THROUGH height=%d hash=%s persisted_index=absent freezer_head=0", w.number, w.headerHash)
}

func TestMathGraphEraNoImportRejectsUntrustedWitnesses(t *testing.T) {
	source := mathgraphLoadEraSource(t)
	t.Run("MissingHashAnchor", func(t *testing.T) {
		db := mathgraphFreshEraDatabase(t)
		w := mathgraphPinnedWitness
		w.headerHash = common.Hash{}
		v, err := mathgraphOpenReadOnlyWitnessView(db, source, w)
		if err == nil || v != nil { t.Fatal("unanchored ERA data became visible") }
	})
	t.Run("IncorrectHashAnchor", func(t *testing.T) {
		db := mathgraphFreshEraDatabase(t)
		w := mathgraphPinnedWitness
		w.headerHash[3] ^= 1
		v, err := mathgraphOpenReadOnlyWitnessView(db, source, w)
		if err == nil || v != nil { t.Fatal("mismatched header hash admitted") }
	})
	t.Run("ReceiptMismatch", func(t *testing.T) {
		db := mathgraphFreshEraDatabase(t)
		s := source
		s.receipts = bytes.Clone(source.receipts)
		s.receipts[0] ^= 1
		v, err := mathgraphOpenReadOnlyWitnessView(db, s, mathgraphPinnedWitness)
		if err == nil || v != nil { t.Fatal("unverified receipts admitted") }
	})
	t.Run("ExistingBackingCanonicalMismatch", func(t *testing.T) {
		db := mathgraphFreshEraDatabase(t)
		other := common.Hash{0xee}
		rawdb.WriteCanonicalHash(db, other, mathgraphEraBlockNumber)
		v, err := mathgraphOpenReadOnlyWitnessView(db, source, mathgraphPinnedWitness)
		if err == nil || v != nil { t.Fatal("view concealed a conflicting canonical index") }
		if got := rawdb.ReadCanonicalHash(db, mathgraphEraBlockNumber); got != other {
			t.Fatal("rejection changed the original canonical index")
		}
	})
	t.Log("UNVERIFIED_ERA_READ_THROUGH_DENIED all negative controls remained invisible")
}
