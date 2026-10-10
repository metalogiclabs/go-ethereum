// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph research-only boundary experiment for go-ethereum issue #35354.
// A caller-supplied exact block witness is not a network consensus oracle:
// it certifies only the block and component commitments checked below.
// This helper lives in a _test.go file and is NOT installed in node startup.
package rawdb_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/rawdb/eradb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/internal/era/onedb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const mathgraphEraBlockNumber uint64 = 175881

type mathgraphEraWitness struct {
	number         uint64
	headerHash     common.Hash
	bodySHA256     [32]byte
	receiptsSHA256 [32]byte
}

// Witness constants are pinned from independently sealed fixture-extraction
// CI 38007909406. They establish fixture identity, not global canonicality.
var mathgraphPinnedWitness = mathgraphEraWitness{
	number:         mathgraphEraBlockNumber,
	headerHash:     common.HexToHash("0x39723cd3caf2b11067d5a95564c802ed6504bb48ed3e70bb7ebff341d181ca13"),
	bodySHA256:     [32]byte(common.HexToHash("0x95cf61df887f2cac1282260b5149771e3793a2b1c24998c889f9188733aa786e")),
	receiptsSHA256: [32]byte(common.HexToHash("0x2b5a8bd32a53ec4a001f6947b8a5219381350b3956941dca592489c6f8ad555a")),
}

type mathgraphEraSource struct {
	block    *types.Block
	body     []byte // exact RLP bytes returned by the ERA reader
	receipts []byte // geth storage RLP after existing ERA conversion
}

func mathgraphLoadEraSource(t *testing.T) mathgraphEraSource {
	t.Helper()
	dir := filepath.Join("eradb", "testdata")
	archive, err := onedb.Open(filepath.Join(dir, "sepolia-00021-b8814b14.era1"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()

	block, err := archive.GetBlockByNumber(mathgraphEraBlockNumber)
	if err != nil {
		t.Fatal(err)
	}
	store, err := eradb.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body, err := store.GetRawBody(mathgraphEraBlockNumber)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := store.GetRawReceipts(mathgraphEraBlockNumber)
	if err != nil {
		t.Fatal(err)
	}
	return mathgraphEraSource{block: block, body: body, receipts: receipts}
}

func mathgraphFreshEraDatabase(t *testing.T) ethdb.Database {
	t.Helper()
	db, err := rawdb.Open(memorydb.New(), rawdb.OpenOptions{
		Ancient: filepath.Join(t.TempDir(), "ancients"),
		Era:     filepath.Join("eradb", "testdata"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fresh database: %v", err)
		}
	})
	return db
}

// mathgraphMaterializeOneVerifiedEraBlock is deliberately test-only and
// validates all witnesses before atomically writing the four normal rawdb
// records. It does not claim an entire canonical interval is present.
func mathgraphMaterializeOneVerifiedEraBlock(db ethdb.Database, source mathgraphEraSource, witness mathgraphEraWitness) error {
	if witness.headerHash == (common.Hash{}) ||
		witness.bodySHA256 == ([32]byte{}) ||
		witness.receiptsSHA256 == ([32]byte{}) {
		return errors.New("missing external block/component witness")
	}
	if source.block == nil || source.block.NumberU64() != witness.number {
		return fmt.Errorf("archive block number disagrees with witness height %d", witness.number)
	}
	header := source.block.Header()
	if header.Hash() != witness.headerHash {
		return fmt.Errorf("archive header hash disagrees with witness at %d", witness.number)
	}
	if sha256.Sum256(source.body) != witness.bodySHA256 ||
		sha256.Sum256(source.receipts) != witness.receiptsSHA256 {
		return errors.New("archive component digest disagrees with witness")
	}

	var body types.Body
	if err := rlp.DecodeBytes(source.body, &body); err != nil {
		return fmt.Errorf("invalid archived body RLP: %w", err)
	}
	encodedBlockBody, err := rlp.EncodeToBytes(source.block.Body())
	if err != nil || !bytes.Equal(encodedBlockBody, source.body) {
		return fmt.Errorf("archive block and raw-body reader disagree: %v", err)
	}
	if want := types.DeriveSha(types.Transactions(body.Transactions), trie.NewStackTrie(nil)); want != header.TxHash {
		return fmt.Errorf("transactions root mismatch: got %s, expected %s", want, header.TxHash)
	}
	if want := types.CalcUncleHash(body.Uncles); want != header.UncleHash {
		return fmt.Errorf("uncle root mismatch: got %s, expected %s", want, header.UncleHash)
	}
	if header.WithdrawalsHash == nil {
		if body.Withdrawals != nil {
			return errors.New("unexpected withdrawals in pre-withdrawals header")
		}
	} else {
		if body.Withdrawals == nil {
			return errors.New("missing withdrawals for header with withdrawals commitment")
		}
		if want := types.DeriveSha(types.Withdrawals(body.Withdrawals), trie.NewStackTrie(nil)); want != *header.WithdrawalsHash {
			return fmt.Errorf("withdrawals root mismatch: got %s, expected %s", want, *header.WithdrawalsHash)
		}
	}

	var stored []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(source.receipts, &stored); err != nil {
		return fmt.Errorf("invalid archived receipts RLP: %w", err)
	}
	if len(stored) != len(body.Transactions) {
		return fmt.Errorf("receipt/transaction count mismatch: %d != %d", len(stored), len(body.Transactions))
	}
	receipts := make(types.Receipts, len(stored))
	for i, sr := range stored {
		if sr == nil {
			return fmt.Errorf("nil receipt at index %d", i)
		}
		r := (*types.Receipt)(sr)
		r.Type = body.Transactions[i].Type()
		r.Bloom = types.CreateBloom(r)
		receipts[i] = r
	}
	if want := types.DeriveSha(receipts, trie.NewStackTrie(nil)); want != header.ReceiptHash {
		return fmt.Errorf("receipts root mismatch: got %s, expected %s", want, header.ReceiptHash)
	}
	if want := types.MergeBloom(receipts); want != header.Bloom {
		return errors.New("block bloom mismatch")
	}

	// Reject an existing index, even if identical: this experiment never
	// mutates an already initialized canonical position.
	if hash := rawdb.ReadCanonicalHash(db, witness.number); hash != (common.Hash{}) {
		return fmt.Errorf("canonical height %d already populated: %s", witness.number, hash)
	}
	batch := db.NewBatch()
	rawdb.WriteHeader(batch, header)
	rawdb.WriteBodyRLP(batch, witness.headerHash, witness.number, source.body)
	rawdb.WriteRawReceipts(batch, witness.headerHash, witness.number, source.receipts)
	rawdb.WriteCanonicalHash(batch, witness.headerHash, witness.number)
	return batch.Write()
}

func mathgraphRequireNoWitnessWrite(t *testing.T, db ethdb.Database, w mathgraphEraWitness, hash common.Hash) {
	t.Helper()
	if got := rawdb.ReadCanonicalHash(db, w.number); got != (common.Hash{}) {
		t.Fatalf("failed import changed canonical hash to %s", got)
	}
	if len(rawdb.ReadHeaderRLP(db, hash, w.number)) != 0 ||
		len(rawdb.ReadBodyRLP(db, hash, w.number)) != 0 ||
		len(rawdb.ReadReceiptsRLP(db, hash, w.number)) != 0 {
		t.Fatal("failed import left an orphaned header/body/receipt")
	}
}

func TestMathGraphVerifiedEraSingleBlockReadThrough(t *testing.T) {
	source := mathgraphLoadEraSource(t)
	db := mathgraphFreshEraDatabase(t)
	w := mathgraphPinnedWitness
	if hash := rawdb.ReadCanonicalHash(db, w.number); hash != (common.Hash{}) {
		t.Fatalf("expected fresh canonical miss, got %s", hash)
	}
	if got := rawdb.ReadCanonicalBodyRLP(db, w.number, nil); len(got) != 0 {
		t.Fatal("unexpected canonical body before certified materialization")
	}
	if err := mathgraphMaterializeOneVerifiedEraBlock(db, source, w); err != nil {
		t.Fatal(err)
	}
	if got := rawdb.ReadCanonicalHash(db, w.number); got != w.headerHash {
		t.Fatalf("read-through canonical hash = %s, expected %s", got, w.headerHash)
	}
	gotHeader := rawdb.ReadHeader(db, w.headerHash, w.number)
	if gotHeader == nil || gotHeader.Hash() != w.headerHash {
		t.Fatal("canonical indexed header is unavailable")
	}
	if got := rawdb.ReadCanonicalBodyRLP(db, w.number, nil); !bytes.Equal(got, source.body) {
		t.Fatal("canonical read does not reproduce witness body bytes")
	}
	if got := rawdb.ReadCanonicalReceiptsRLP(db, w.number, nil); !bytes.Equal(got, source.receipts) {
		t.Fatal("canonical read does not reproduce witness receipt bytes")
	}
	if got := rawdb.ReadBodyRLP(db, w.headerHash, w.number); !bytes.Equal(got, source.body) {
		t.Fatal("hash-addressed body read failed")
	}
	if got := rawdb.ReadReceiptsRLP(db, w.headerHash, w.number); !bytes.Equal(got, source.receipts) {
		t.Fatal("hash-addressed receipt read failed")
	}
	if got := rawdb.ReadBlock(db, w.headerHash, w.number); got == nil || got.Hash() != w.headerHash {
		t.Fatal("read-through block reconstruction failed")
	}
	if got := rawdb.ReadCanonicalHash(db, w.number-1); got != (common.Hash{}) {
		t.Fatalf("invented preceding canonical block: %s", got)
	}
	if got := rawdb.ReadCanonicalHash(db, w.number+1); got != (common.Hash{}) {
		t.Fatalf("invented following canonical block: %s", got)
	}
	if frozen, err := db.Ancients(); err != nil || frozen != 0 {
		t.Fatalf("single-block witness must not promote an entire ancient interval: count=%d err=%v", frozen, err)
	}
	// Idempotence is not assumed. A second import must not silently rewrite.
	if err := mathgraphMaterializeOneVerifiedEraBlock(db, source, w); err == nil {
		t.Fatal("repeated witness import unexpectedly rewrote canonical height")
	}
	t.Logf("VERIFIED_SINGLE_ERA_BLOCK_READ_THROUGH number=%d hash=%s body=%d receipts=%d ancient_head=0", w.number, w.headerHash, len(source.body), len(source.receipts))
}

func TestMathGraphVerifiedEraBoundaryRejectsUntrustedHistory(t *testing.T) {
	source := mathgraphLoadEraSource(t)
	cases := []struct {
		name string
		edit func(*mathgraphEraSource, *mathgraphEraWitness)
	}{
		{"NoHeaderAnchor", func(_ *mathgraphEraSource, w *mathgraphEraWitness) {
			w.headerHash = common.Hash{}
		}},
		{"WrongHeaderAnchor", func(_ *mathgraphEraSource, w *mathgraphEraWitness) {
			w.headerHash[0] ^= 1
		}},
		{"WrongHeight", func(_ *mathgraphEraSource, w *mathgraphEraWitness) {
			w.number++
		}},
		{"BodyDigestMismatch", func(_ *mathgraphEraSource, w *mathgraphEraWitness) {
			w.bodySHA256[0] ^= 1
		}},
		{"ReceiptDigestMismatch", func(_ *mathgraphEraSource, w *mathgraphEraWitness) {
			w.receiptsSHA256[0] ^= 1
		}},
		{"CorruptRawBody", func(s *mathgraphEraSource, _ *mathgraphEraWitness) {
			s.body = append(append([]byte{}, s.body...), 0x00)
		}},
		{"CorruptRawReceipt", func(s *mathgraphEraSource, _ *mathgraphEraWitness) {
			s.receipts = append(append([]byte{}, s.receipts...), 0x00)
		}},
		{"BadBodyEvenWithUpdatedDigest", func(s *mathgraphEraSource, w *mathgraphEraWitness) {
			badBody := s.block.Body()
			badBody.Transactions = badBody.Transactions[:0]
			s.block = s.block.WithBody(*badBody)
			encoded, err := rlp.EncodeToBytes(badBody)
			if err != nil { panic(err) }
			s.body = encoded
			w.bodySHA256 = sha256.Sum256(encoded)
		}},
		{"BadReceiptEvenWithUpdatedDigest", func(s *mathgraphEraSource, w *mathgraphEraWitness) {
			s.receipts = []byte{0xc0}
			w.receiptsSHA256 = sha256.Sum256(s.receipts)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := mathgraphFreshEraDatabase(t)
			s := mathgraphEraSource{
				block: source.block,
				body: append([]byte{}, source.body...),
				receipts: append([]byte{}, source.receipts...),
			}
			w := mathgraphPinnedWitness
			tc.edit(&s, &w)
			if err := mathgraphMaterializeOneVerifiedEraBlock(db, s, w); err == nil {
				t.Fatal("unverified or mismatched history was admitted")
			}
			mathgraphRequireNoWitnessWrite(t, db, mathgraphPinnedWitness, mathgraphPinnedWitness.headerHash)
		})
	}
	t.Run("ExistingDifferentCanonicalAnchor", func(t *testing.T) {
		db := mathgraphFreshEraDatabase(t)
		existing := common.Hash{0xfa}
		rawdb.WriteCanonicalHash(db, existing, mathgraphEraBlockNumber)
		if err := mathgraphMaterializeOneVerifiedEraBlock(db, source, mathgraphPinnedWitness); err == nil {
			t.Fatal("materializer overwrote conflicting canonical entry")
		}
		if got := rawdb.ReadCanonicalHash(db, mathgraphEraBlockNumber); got != existing {
			t.Fatalf("conflicting canonical anchor was changed to %s", got)
		}
		if got := rawdb.ReadBodyRLP(db, mathgraphPinnedWitness.headerHash, mathgraphEraBlockNumber); len(got) != 0 {
			t.Fatal("conflict rejection left candidate body")
		}
	})
	t.Log("UNTRUSTED_ERA_HISTORY_REJECTED all wrong anchors, digests, decoded commitments and conflicting canonical entries")
}
