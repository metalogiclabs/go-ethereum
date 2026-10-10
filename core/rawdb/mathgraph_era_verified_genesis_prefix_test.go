// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph test-only experiment for go-ethereum issue #35354:
// expose an authenticated PREFIX beginning at Sepolia genesis, as distinct
// from an isolated certified historical interval. No production code changes.
package rawdb_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/rawdb/eradb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/era/onedb"
)

const (
	mathgraphGenesisPrefixLength = 64
	// SHA-256 of Sepolia epoch-0 ERA1 file, published at:
	// https://data.ethpandaops.io/era1/sepolia/checksums.txt
	// This is an archive publisher checksum, not proof of consensus finality.
	mathgraphPublishedEra0SHA256 = "ab7f6d4f4eba0267406f783b75accc7a93dece520242d04fed27b0af51d79242"
)

var mathgraphPublishedSepoliaGenesis = common.HexToHash("0x25a5cc106eea7138acab33231d7160d69cb777ee0c2c553fcddf5138993e6dd9")

// A prefix view has a different Ancient HEAD invariant than an isolated
// segment: [0,count) is covered without gaps. It is still ONLY a test reader.
type mathgraphGenesisPrefixView struct {
	*mathgraphEraSegmentView
}

func (v *mathgraphGenesisPrefixView) Ancients() (uint64, error) {
	return v.last + 1, nil
}

func (v *mathgraphGenesisPrefixView) ReadAncients(fn func(ethdb.AncientReaderOp) error) error {
	// Must expose the prefix-aware reader, not the embedded segment, inside
	// the lock. Otherwise ReadCanonicalHash and range readers disagree.
	return v.mathgraphEraSegmentView.Database.ReadAncients(func(_ ethdb.AncientReaderOp) error {
		return fn(v)
	})
}

func mathgraphEra0Path() string {
	return filepath.Join("eradb", "testdata", "sepolia-00000-643a00f7.era1")
}

// Verify the entire input-file bytes against a separate publisher manifest
// BEFORE allowing an artificial historical prefix to be constructed.
func mathgraphVerifyPublishedEra0Archive() error {
	f, err := os.Open(mathgraphEra0Path())
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != mathgraphPublishedEra0SHA256 {
		return fmt.Errorf("Sepolia ERA0 archive publisher SHA-256 mismatch: %s", got)
	}
	return nil
}

func mathgraphLoadGenesisPrefix(t *testing.T, length int) []mathgraphEraSource {
	t.Helper()
	if err := mathgraphVerifyPublishedEra0Archive(); err != nil {
		t.Fatal(err)
	}
	archive, err := onedb.Open(mathgraphEra0Path())
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	store, err := eradb.New(filepath.Join("eradb", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sources := make([]mathgraphEraSource, length)
	for i := 0; i < length; i++ {
		n := uint64(i)
		block, err := archive.GetBlockByNumber(n)
		if err != nil {
			t.Fatal(err)
		}
		body, err := store.GetRawBody(n)
		if err != nil {
			t.Fatal(err)
		}
		receipts, err := store.GetRawReceipts(n)
		if err != nil {
			t.Fatal(err)
		}
		sources[i] = mathgraphEraSource{block: block, body: body, receipts: receipts}
	}
	return sources
}

// This constructor is intentionally conservative and limited to 64 blocks.
// It establishes a virtual [0,count) ancient prefix ONLY when the file SHA,
// published genesis, exact sequential headers, header hash links, transaction
// trie commitments and receipt commitments are all accepted. No backing
// storage is modified; no execution state is reconstructed.
func mathgraphOpenVerifiedGenesisPrefix(db ethdb.Database, sources []mathgraphEraSource) (*mathgraphGenesisPrefixView, error) {
	if len(sources) < 2 || len(sources) > mathgraphGenesisPrefixLength {
		return nil, fmt.Errorf("uncertified genesis prefix size %d", len(sources))
	}
	if err := mathgraphVerifyPublishedEra0Archive(); err != nil {
		return nil, err
	}
	if sources[0].block == nil || sources[0].block.NumberU64() != 0 ||
		sources[0].block.Hash() != mathgraphPublishedSepoliaGenesis {
		return nil, errors.New("genesis header differs from published Sepolia genesis")
	}
	if sources[0].block.ParentHash() != (common.Hash{}) {
		return nil, errors.New("genesis has unexpected parent")
	}
	if n, err := db.Ancients(); err != nil || n != 0 {
		return nil, fmt.Errorf("backing database is not a fresh ancient store: count=%d err=%v", n, err)
	}
	for i, s := range sources {
		n := uint64(i)
		if s.block == nil || s.block.NumberU64() != n {
			return nil, fmt.Errorf("gap or misplaced archived block at %d", n)
		}
		if i > 0 && s.block.ParentHash() != sources[i-1].block.Hash() {
			return nil, fmt.Errorf("broken canonical parent relation at %d", n)
		}
		if canonical := rawdb.ReadCanonicalHash(db, n); canonical != (common.Hash{}) {
			return nil, fmt.Errorf("existing canonical block at %d is not virtual", n)
		}
		if len(rawdb.ReadHeaderRLP(db, s.block.Hash(), n)) != 0 ||
			len(rawdb.ReadBodyRLP(db, s.block.Hash(), n)) != 0 ||
			len(rawdb.ReadReceiptsRLP(db, s.block.Hash(), n)) != 0 {
			return nil, fmt.Errorf("existing orphaned DB component at %d", n)
		}
	}

	scratch := rawdb.NewMemoryDatabase()
	defer scratch.Close()
	verified := make(map[uint64]map[string][]byte, len(sources))
	for i, s := range sources {
		n := uint64(i)
		witness := mathgraphEraWitness{
			number: n,
			headerHash: s.block.Hash(),
			bodySHA256: sha256.Sum256(s.body),
			receiptsSHA256: sha256.Sum256(s.receipts),
		}
		if err := mathgraphMaterializeOneVerifiedEraBlock(scratch, s, witness); err != nil {
			return nil, fmt.Errorf("ERA genesis-prefix block %d invalid: %w", n, err)
		}
		header := rawdb.ReadHeaderRLP(scratch, witness.headerHash, n)
		body := rawdb.ReadBodyRLP(scratch, witness.headerHash, n)
		receipts := rawdb.ReadReceiptsRLP(scratch, witness.headerHash, n)
		if len(header) == 0 || len(body) == 0 || len(receipts) == 0 {
			return nil, fmt.Errorf("unavailable verified prefix component at %d", n)
		}
		verified[n] = map[string][]byte{
			rawdb.ChainFreezerHashTable: bytes.Clone(witness.headerHash[:]),
			rawdb.ChainFreezerHeaderTable: bytes.Clone(header),
			rawdb.ChainFreezerBodiesTable: bytes.Clone(body),
			rawdb.ChainFreezerReceiptTable: bytes.Clone(receipts),
		}
	}
	return &mathgraphGenesisPrefixView{&mathgraphEraSegmentView{
		Database: db, first: 0, last: uint64(len(sources)-1), certified: verified,
	}}, nil
}

func TestMathGraphEraVerifiedGenesisPrefixReads(t *testing.T) {
	source := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	db := mathgraphFreshEraDatabase(t)
	view, err := mathgraphOpenVerifiedGenesisPrefix(db, source)
	if err != nil {
		t.Fatal(err)
	}
	head, err := view.Ancients()
	if err != nil || head != mathgraphGenesisPrefixLength {
		t.Fatalf("incorrect prefix ancient head %d err=%v", head, err)
	}
	rlpHeaders := rawdb.ReadHeaderRange(view, mathgraphGenesisPrefixLength-1, mathgraphGenesisPrefixLength)
	if len(rlpHeaders) != mathgraphGenesisPrefixLength {
		t.Fatalf("existing header-range reader returned only %d verified prefix headers", len(rlpHeaders))
	}
	for i, s := range source {
		n := uint64(i)
		hash := s.block.Hash()
		if got := rawdb.ReadCanonicalHash(view, n); got != hash {
			t.Fatalf("missing virtual canonical prefix at %d", n)
		}
		if got := rawdb.ReadHeader(view, hash, n); got == nil || got.Hash() != hash {
			t.Fatalf("header absent at %d", n)
		}
		if got := rawdb.ReadCanonicalBodyRLP(view, n, nil); !bytes.Equal(got, s.body) {
			t.Fatalf("body absent at %d", n)
		}
		if got := rawdb.ReadCanonicalReceiptsRLP(view, n, nil); !bytes.Equal(got, s.receipts) {
			t.Fatalf("receipt absent at %d", n)
		}
		if crypto.Keccak256Hash(rlpHeaders[mathgraphGenesisPrefixLength-1-i]) != hash {
			t.Fatalf("header order differs from existing reader semantics at %d", n)
		}
		if rawdb.ReadCanonicalHash(db, n) != (common.Hash{}) {
			t.Fatalf("virtual ancient prefix leaked a backing index at %d", n)
		}
	}
	if got := rawdb.ReadCanonicalHash(view, mathgraphGenesisPrefixLength); got != (common.Hash{}) {
		t.Fatal("unverified block beyond virtual ancient head became canonical")
	}
	if frozen, err := db.Ancients(); err != nil || frozen != 0 {
		t.Fatalf("backing database was modified: head=%d err=%v", frozen, err)
	}
	t.Logf("VERIFIED_ERA_GENESIS_VIRTUAL_PREFIX count=%d genesis=%s publisher_sha256=%s backing_head=0",
		mathgraphGenesisPrefixLength, source[0].block.Hash(), mathgraphPublishedEra0SHA256)
}

func TestMathGraphEraGenesisPrefixRejectsIncompleteHistory(t *testing.T) {
	original := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	cases := []struct{
		name string
		edit func([]mathgraphEraSource) []mathgraphEraSource
	}{
		{"MissingGenesis",func(s []mathgraphEraSource) []mathgraphEraSource {return s[1:]}},
		{"MissingInterior",func(s []mathgraphEraSource) []mathgraphEraSource {return append(s[:22:22],s[23:]...)}},
		{"DuplicateInterior",func(s []mathgraphEraSource) []mathgraphEraSource {s[19]=s[18];return s}},
		{"SwappedInterior",func(s []mathgraphEraSource) []mathgraphEraSource {s[31],s[32]=s[32],s[31];return s}},
		{"CorruptInteriorBody",func(s []mathgraphEraSource) []mathgraphEraSource {
			s[23].body=append(bytes.Clone(s[23].body),0x00)
			return s
		}},
		{"CorruptInteriorReceipts",func(s []mathgraphEraSource) []mathgraphEraSource {
			s[47].receipts=append(bytes.Clone(s[47].receipts),0x00)
			return s
		}},
	}
	for _,tc:=range cases {
		t.Run(tc.name,func(t *testing.T){
			db:=mathgraphFreshEraDatabase(t)
			edited:=tc.edit(append([]mathgraphEraSource(nil),original...))
			v,err:=mathgraphOpenVerifiedGenesisPrefix(db,edited)
			if err==nil||v!=nil {t.Fatal("incomplete or corrupted prefix was accepted")}
			if got:=rawdb.ReadCanonicalHash(db,0);got!=(common.Hash{}) {t.Fatal("rejected prefix mutated genesis DB")}
		})
	}
	t.Run("ConflictingBackingCanonical",func(t *testing.T){
		db:=mathgraphFreshEraDatabase(t)
		existing:=common.Hash{0xbb}
		rawdb.WriteCanonicalHash(db,existing,21)
		v,err:=mathgraphOpenVerifiedGenesisPrefix(db,original)
		if err==nil||v!=nil {t.Fatal("existing canonical block was hidden by archive prefix")}
		if got:=rawdb.ReadCanonicalHash(db,21);got!=existing {t.Fatal("rejection altered backing canonical block")}
	})
	t.Run("ExistingOrphanedHeader",func(t *testing.T){
		db:=mathgraphFreshEraDatabase(t)
		rawdb.WriteHeader(db,original[15].block.Header())
		v,err:=mathgraphOpenVerifiedGenesisPrefix(db,original)
		if err==nil||v!=nil {t.Fatal("orphaned header was masked")}
	})
	t.Log("REJECTED_ERA_GENESIS_PREFIX_GAP_CORRUPTION_AND_INDEX_CONFLICT")
}

func TestMathGraphEraGenesisPrefixConcurrent(t *testing.T) {
	source:=mathgraphLoadGenesisPrefix(t,mathgraphGenesisPrefixLength)
	db:=mathgraphFreshEraDatabase(t)
	view,err:=mathgraphOpenVerifiedGenesisPrefix(db,source)
	if err!=nil {t.Fatal(err)}
	const workers=6
	var wg sync.WaitGroup
	errs:=make(chan error,workers)
	for j:=0;j<workers;j++ {
		wg.Add(1)
		go func(worker int){
			defer wg.Done()
			for i:=0;i<20;i++ {
				idx:=(worker*11+i*13)%mathgraphGenesisPrefixLength
				n:=uint64(idx)
				if got:=rawdb.ReadCanonicalHash(view,n);got!=source[idx].block.Hash() {
					errs<-fmt.Errorf("virtual prefix mismatch at %d",n)
					return
				}
				if got:=rawdb.ReadCanonicalReceiptsRLP(view,n,nil);!bytes.Equal(got,source[idx].receipts) {
					errs<-fmt.Errorf("virtual receipt mismatch at %d",n)
					return
				}
			}
		}(j)
	}
	wg.Wait()
	close(errs)
	for err:=range errs {t.Error(err)}
	t.Log("VERIFIED_ERA_GENESIS_PREFIX_CONCURRENT_READERS")
}
