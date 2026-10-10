// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph experiment for ethereum/go-ethereum#35354.
// A bounded, read-only ERA segment is admitted ONLY after every source block
// is checked against the authenticated parent chain ending in a pinned tip.
// The pinned tip authenticates this TEST FIXTURE, not Sepolia finality.
// The overlay never modifies the live canonical index or claims a prefix
// of the ancient database; all implementation is test-only.
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
    "github.com/ethereum/go-ethereum/crypto"
    "github.com/ethereum/go-ethereum/ethdb"
    "github.com/ethereum/go-ethereum/internal/era/onedb"
)

type mathgraphEraSegmentView struct {
    ethdb.Database
    first uint64
    last uint64
    certified map[uint64]map[string][]byte // immutable after construction
}

func (v *mathgraphEraSegmentView) Ancient(kind string, number uint64) ([]byte, error) {
    if number >= v.first && number <= v.last {
        entry, exists := v.certified[number]
        if !exists {
            return nil, fmt.Errorf("unverified hole at ERA block %d", number)
        }
        data, ok := entry[kind]
        if !ok {
            return nil, fmt.Errorf("ERA component %q not warranted for block %d", kind, number)
        }
        return bytes.Clone(data), nil
    }
    return v.Database.Ancient(kind, number)
}

func (v *mathgraphEraSegmentView) ReadAncients(fn func(ethdb.AncientReaderOp) error) error {
    // Hold the backing freezer read lock while presenting the bounded view.
    return v.Database.ReadAncients(func(_ ethdb.AncientReaderOp) error { return fn(v) })
}

func (v *mathgraphEraSegmentView) AncientRange(kind string, start, count, maxBytes uint64) ([][]byte, error) {
    if count == 0 {
        return [][]byte{}, nil
    }
    // Within our bounded segment, demand the FULL requested range.
    // In particular never concatenate a verified ERA span with unchecked
    // freezer history, silently treat a hole as zero, or wrap uint64.
    if start >= v.first && start <= v.last {
        if count > v.last-start+1 {
            return nil, fmt.Errorf("ERA segment [%d,%d] cannot warrant %d blocks from %d",
                v.first, v.last, count, start)
        }
        var (
            result [][]byte
            total uint64
        )
        for i := uint64(0); i < count; i++ {
            chunk, err := v.Ancient(kind, start+i)
            if err != nil {
                return nil, err
            }
            if maxBytes != 0 && len(result) != 0 && total+uint64(len(chunk)) > maxBytes {
                break
            }
            result = append(result, chunk)
            total += uint64(len(chunk))
        }
        return result, nil
    }
    // A range starting outside but crossing our overlay is forbidden.
    if start < v.first && count > v.first-start {
        return nil, errors.New("refusing partially certified ERA range")
    }
    return v.Database.AncientRange(kind, start, count, maxBytes)
}

func mathgraphLoadEraSources(t *testing.T, numbers ...uint64) []mathgraphEraSource {
    t.Helper()
    archive, err := onedb.Open(filepath.Join("eradb", "testdata", "sepolia-00021-b8814b14.era1"))
    if err != nil { t.Fatal(err) }
    defer archive.Close()
    reader, err := eradb.New(filepath.Join("eradb", "testdata"))
    if err != nil { t.Fatal(err) }
    defer reader.Close()

    sources := make([]mathgraphEraSource, len(numbers))
    for i, number := range numbers {
        block, err := archive.GetBlockByNumber(number)
        if err != nil { t.Fatalf("ERA header %d: %v", number, err) }
        body, err := reader.GetRawBody(number)
        if err != nil { t.Fatalf("ERA body %d: %v", number, err) }
        receipts, err := reader.GetRawReceipts(number)
        if err != nil { t.Fatalf("ERA receipts %d: %v", number, err) }
        sources[i] = mathgraphEraSource{block: block, body: body, receipts: receipts}
    }
    return sources
}

// mathgraphOpenAnchoredEraSegment verifies every record on a scratch DB before
// publishing ANY virtual index to the caller. The only externally pinned
// fixture record is the tip; all earlier hashes are authenticated by
// following its parents. Their SHA-256 digests are local corruption controls;
// the authoritative body/receipt commitments are checked by the block header.
func mathgraphOpenAnchoredEraSegment(db ethdb.Database, sources []mathgraphEraSource, tip mathgraphEraWitness) (*mathgraphEraSegmentView, error) {
    if len(sources) < 2 || len(sources) > 64 {
        return nil, fmt.Errorf("bounded segment needs 2 to 64 blocks, got %d", len(sources))
    }
    if tip.number != mathgraphPinnedWitness.number ||
        tip.headerHash != mathgraphPinnedWitness.headerHash ||
        tip.bodySHA256 != mathgraphPinnedWitness.bodySHA256 ||
        tip.receiptsSHA256 != mathgraphPinnedWitness.receiptsSHA256 {
        return nil, errors.New("unrecognized or unanchored segment endpoint")
    }
    first := tip.number - uint64(len(sources)-1)
    // Verify the graph from the pinned tip toward its predecessors before
    // allowing the data into a read view. Height is part of the identity.
    expect := tip.headerHash
    for i := len(sources)-1; i >= 0; i-- {
        number := first + uint64(i)
        block := sources[i].block
        if block == nil || block.NumberU64() != number {
            return nil, fmt.Errorf("missing, duplicated or misordered block at height %d", number)
        }
        if block.Hash() != expect {
            return nil, fmt.Errorf("broken parent lineage at %d: got %s, want %s",
                number, block.Hash(), expect)
        }
        expect = block.ParentHash()
    }

    // Fail closed before constructing the overlay if ANY tested position has
    // a conflicting canonical entry or existing orphaned block component.
    for i, source := range sources {
        n := first + uint64(i)
        hash := source.block.Hash()
        if got := rawdb.ReadCanonicalHash(db, n); got != (common.Hash{}) {
            return nil, fmt.Errorf("backing canonical entry at %d: %s", n, got)
        }
        if len(rawdb.ReadHeaderRLP(db, hash, n)) != 0 ||
            len(rawdb.ReadBodyRLP(db, hash, n)) != 0 ||
            len(rawdb.ReadReceiptsRLP(db, hash, n)) != 0 {
            return nil, fmt.Errorf("backing database already contains block components at %d", n)
        }
    }

    scratch := rawdb.NewMemoryDatabase()
    defer scratch.Close()
    certified := make(map[uint64]map[string][]byte, len(sources))
    for i, source := range sources {
        n := first + uint64(i)
        witness := mathgraphEraWitness{
            number: n,
            headerHash: source.block.Hash(),
            bodySHA256: sha256.Sum256(source.body),
            receiptsSHA256: sha256.Sum256(source.receipts),
        }
        if n == tip.number {
            witness = tip // Require the independently sealed fixture digests.
        }
        if err := mathgraphMaterializeOneVerifiedEraBlock(scratch, source, witness); err != nil {
            return nil, fmt.Errorf("ERA block %d failed header/body/receipt validation: %w", n, err)
        }
        header := rawdb.ReadHeaderRLP(scratch, witness.headerHash, n)
        body := rawdb.ReadBodyRLP(scratch, witness.headerHash, n)
        receipts := rawdb.ReadReceiptsRLP(scratch, witness.headerHash, n)
        if len(header) == 0 || len(body) == 0 || len(receipts) == 0 {
            return nil, fmt.Errorf("missing validated ERA component at %d", n)
        }
        certified[n] = map[string][]byte{
            rawdb.ChainFreezerHashTable: bytes.Clone(witness.headerHash[:]),
            rawdb.ChainFreezerHeaderTable: bytes.Clone(header),
            rawdb.ChainFreezerBodiesTable: bytes.Clone(body),
            rawdb.ChainFreezerReceiptTable: bytes.Clone(receipts),
        }
    }
    return &mathgraphEraSegmentView{
        Database: db,
        first: first, last: tip.number, certified: certified,
    }, nil
}

func TestMathGraphEraVerifiedTwoBlockAncestryReadThrough(t *testing.T) {
    const prev uint64 = mathgraphEraBlockNumber - 1
    sources := mathgraphLoadEraSources(t, prev, mathgraphEraBlockNumber)
    db := mathgraphFreshEraDatabase(t)
    view, err := mathgraphOpenAnchoredEraSegment(db, sources, mathgraphPinnedWitness)
    if err != nil { t.Fatal(err) }

    if sources[1].block.ParentHash() != sources[0].block.Hash() {
        t.Fatal("bad test fixture: predecessor does not match pinned tip parent hash")
    }
    for i, source := range sources {
        n := prev + uint64(i)
        hash := source.block.Hash()
        if got := rawdb.ReadCanonicalHash(view, n); got != hash {
            t.Fatalf("virtual canonical hash at %d: %s != %s", n, got, hash)
        }
        if h := rawdb.ReadHeader(view, hash, n); h == nil || h.Hash() != hash {
            t.Fatalf("read header failed at %d", n)
        }
        if got := rawdb.ReadCanonicalBodyRLP(view, n, nil); !bytes.Equal(got, source.body) {
            t.Fatalf("body mismatch at %d", n)
        }
        if got := rawdb.ReadCanonicalReceiptsRLP(view, n, nil); !bytes.Equal(got, source.receipts) {
            t.Fatalf("receipt mismatch at %d", n)
        }
        if got := rawdb.ReadBlock(view, hash, n); got == nil || got.Hash() != hash {
            t.Fatalf("ReadBlock mismatch at %d", n)
        }
        if got := rawdb.ReadBodyRLP(view, common.Hash{0xff}, n); len(got) != 0 {
            t.Fatalf("wrong hash escaped verified view at %d", n)
        }
        if got := rawdb.ReadCanonicalHash(db, n); got != (common.Hash{}) {
            t.Fatalf("read-through persisted a backing canonical hash at %d", n)
        }
        if got := rawdb.ReadCanonicalBodyRLP(db, n, nil); len(got) != 0 {
            t.Fatalf("read-through persisted a backing body at %d", n)
        }
    }

    // Test the ancient range interface and the existing rawdb header-range
    // API, not only single-height lookups.
    headers := rawdb.ReadHeaderRange(view, mathgraphEraBlockNumber, 2)
    if len(headers) != 2 ||
        crypto.Keccak256Hash(headers[0]) != sources[1].block.Hash() ||
        crypto.Keccak256Hash(headers[1]) != sources[0].block.Hash() {
        t.Fatalf("standard reverse-order ReadHeaderRange failed: %d headers", len(headers))
    }
    rawHeaders, err := view.AncientRange(rawdb.ChainFreezerHeaderTable, prev, 2, 0)
    if err != nil || len(rawHeaders) != 2 {
        t.Fatalf("verified ascending header range absent: %d headers, err=%v", len(rawHeaders), err)
    }
    if crypto.Keccak256Hash(rawHeaders[0]) != sources[0].block.Hash() ||
        crypto.Keccak256Hash(rawHeaders[1]) != sources[1].block.Hash() {
        t.Fatal("verified header range out of order")
    }
    one, err := view.AncientRange(rawdb.ChainFreezerHeaderTable, prev, 2, 1)
    if err != nil || len(one) != 1 {
        t.Fatalf("AncientRange byte-cap must preserve first item: count=%d err=%v", len(one), err)
    }
    if _, err := view.AncientRange(rawdb.ChainFreezerHeaderTable, prev-1, 3, 0); err == nil {
        t.Fatal("a request spanning unverified history was silently allowed")
    }
    if _, err := view.AncientRange(rawdb.ChainFreezerHeaderTable, prev, 3, 0); err == nil {
        t.Fatal("a request extending past the authenticated tip was silently allowed")
    }
    if got := rawdb.ReadCanonicalHash(view, prev-1); got != (common.Hash{}) {
        t.Fatal("unverified predecessor invented")
    }
    if got := rawdb.ReadCanonicalHash(view, mathgraphEraBlockNumber+1); got != (common.Hash{}) {
        t.Fatal("unverified successor invented")
    }
    if frozen, err := db.Ancients(); err != nil || frozen != 0 {
        t.Fatalf("test view must not invent canonical genesis prefix: head=%d err=%v", frozen, err)
    }
    t.Logf("VERIFIED_TWO_BLOCK_NO_IMPORT_CHAIN first=%d last=%d tip=%s parent=%s backing_ancients=0",
        prev, mathgraphEraBlockNumber, sources[1].block.Hash(), sources[0].block.Hash())
}

func TestMathGraphEraVerifiedSegmentRejectsBrokenLineage(t *testing.T) {
    const first uint64 = mathgraphEraBlockNumber - 1
    source := mathgraphLoadEraSources(t, first, mathgraphEraBlockNumber)
    cases := []struct{
        name string
        change func([]mathgraphEraSource)
    }{
        {"MissingPredecessor", func(s []mathgraphEraSource) { s[0].block = nil }},
        {"DuplicateTipAtPredecessorHeight", func(s []mathgraphEraSource) { s[0] = s[1] }},
        {"ReversedInputOrder", func(s []mathgraphEraSource) { s[0], s[1] = s[1], s[0] }},
        {"ForkAtPredecessor", func(s []mathgraphEraSource) {
            h := types.CopyHeader(s[0].block.Header())
            h.Extra = append(h.Extra, 0x75)
            s[0].block = types.NewBlockWithHeader(h).WithBody(*s[0].block.Body())
        }},
        {"CorruptPredecessorBody", func(s []mathgraphEraSource) {
            s[0].body = append(bytes.Clone(s[0].body), 0x00)
        }},
        {"CorruptTipReceipts", func(s []mathgraphEraSource) {
            s[1].receipts = append(bytes.Clone(s[1].receipts), 0x00)
        }},
        {"EmptyPredecessorReceiptData", func(s []mathgraphEraSource) {
            s[0].receipts = nil
        }},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            entries := append([]mathgraphEraSource(nil), source...)
            tc.change(entries)
            db := mathgraphFreshEraDatabase(t)
            v, err := mathgraphOpenAnchoredEraSegment(db, entries, mathgraphPinnedWitness)
            if err == nil || v != nil {
                t.Fatal("invalid ancestry or block data became visible")
            }
            for _, n := range []uint64{first, mathgraphEraBlockNumber} {
                if got := rawdb.ReadCanonicalHash(db, n); got != (common.Hash{}) {
                    t.Fatalf("rejected history changed backing canonical height %d", n)
                }
            }
        })
    }
    t.Run("MissingMiddleOfThree", func(t *testing.T) {
        sources := mathgraphLoadEraSources(t, first-1, mathgraphEraBlockNumber)
        db := mathgraphFreshEraDatabase(t)
        v, err := mathgraphOpenAnchoredEraSegment(db, sources, mathgraphPinnedWitness)
        if err == nil || v != nil {
            t.Fatal("noncontiguous three-block request was admitted")
        }
    })
    t.Run("UnanchoredEndpoint", func(t *testing.T) {
        db := mathgraphFreshEraDatabase(t)
        w := mathgraphPinnedWitness
        w.headerHash[0] ^= 1
        v, err := mathgraphOpenAnchoredEraSegment(db, source, w)
        if err == nil || v != nil {
            t.Fatal("unrecognized endpoint anchor was admitted")
        }
    })
    t.Run("ConflictingCanonicalAtPredecessor", func(t *testing.T) {
        db := mathgraphFreshEraDatabase(t)
        existing := common.Hash{0x99}
        rawdb.WriteCanonicalHash(db, existing, first)
        v, err := mathgraphOpenAnchoredEraSegment(db, source, mathgraphPinnedWitness)
        if err == nil || v != nil { t.Fatal("conflicting backing history was masked") }
        if got := rawdb.ReadCanonicalHash(db, first); got != existing {
            t.Fatal("rejection modified the backing index")
        }
    })
    t.Run("ConflictingCanonicalAtTip", func(t *testing.T) {
        db := mathgraphFreshEraDatabase(t)
        existing := common.Hash{0x88}
        rawdb.WriteCanonicalHash(db, existing, mathgraphEraBlockNumber)
        v, err := mathgraphOpenAnchoredEraSegment(db, source, mathgraphPinnedWitness)
        if err == nil || v != nil { t.Fatal("conflicting pinned endpoint was masked") }
        if got := rawdb.ReadCanonicalHash(db, mathgraphEraBlockNumber); got != existing {
            t.Fatal("rejection modified the pinned endpoint")
        }
    })
    t.Run("ExistingOrphanedHeader", func(t *testing.T) {
        db := mathgraphFreshEraDatabase(t)
        rawdb.WriteHeader(db, source[0].block.Header())
        v, err := mathgraphOpenAnchoredEraSegment(db, source, mathgraphPinnedWitness)
        if err == nil || v != nil { t.Fatal("existing orphaned header was masked") }
        if h := rawdb.ReadHeader(db, source[0].block.Hash(), first); h == nil {
            t.Fatal("rejection deleted the orphaned header")
        }
    })
    t.Log("REJECTED_UNWARRANTED_ERA_LINEAGE missing/duplicate/fork/gap/corruption/anchor/index/orphan controls")
}

func TestMathGraphEraVerifiedThreeBlockChain(t *testing.T) {
    first := mathgraphEraBlockNumber-2
    source := mathgraphLoadEraSources(t, first, first+1, mathgraphEraBlockNumber)
    db := mathgraphFreshEraDatabase(t)
    view, err := mathgraphOpenAnchoredEraSegment(db, source, mathgraphPinnedWitness)
    if err != nil { t.Fatal(err) }
    got := rawdb.ReadHeaderRange(view, mathgraphEraBlockNumber, 3)
    if len(got) != 3 {
        t.Fatalf("three-block header range failed: received %d headers", len(got))
    }
    for i, original := range source {
        n := first+uint64(i)
        if rawdb.ReadCanonicalHash(view,n) != original.block.Hash() {
            t.Fatalf("three-block canonical read failed at %d",n)
        }
        if rawdb.ReadCanonicalHash(db,n) != (common.Hash{}) {
            t.Fatalf("three-block overlay modified backing index at %d",n)
        }
    }
    if _, err := view.Ancient(rawdb.ChainFreezerBALTable, mathgraphEraBlockNumber); err == nil {
        t.Fatal("unverified block access list was exposed")
    }
    t.Logf("VERIFIED_THREE_BLOCK_ANCESTRY first=%d last=%d no_import=true",first,mathgraphEraBlockNumber)
}
