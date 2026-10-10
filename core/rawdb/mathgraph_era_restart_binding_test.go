// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// MathGraph TEST-ONLY restart guard for a verified ERA1-backed genesis prefix.
// This is a proposed integration boundary, not a Geth startup modification.
// Only a small certificate is stored; archive block bodies and receipts remain
// on disk inside their separately checksummed ERA file.
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
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
)

// Prefix in the TEST ONLY keyspace, never a deployed geth schema claim.
var mathgraphEraBindingKey = []byte("mathgraph/research/verified-era-prefix/v1")

// Smallest durable claim needed to bind an initialized header index to its
// independently checksummed historical source. The tip is a fixture header
// hash, not a consensus checkpoint or finality proof.
type mathgraphEraRestartCertificate struct {
	Version uint64
	Genesis common.Hash
	ArchiveSHA256 common.Hash
	First uint64
	Count uint64
	Tip common.Hash
}

func mathgraphExpectedRestartCertificate(sources []mathgraphEraSource) ([]byte, error) {
	if len(sources) != mathgraphGenesisPrefixLength {
		return nil, errors.New("unexpected certified prefix length")
	}
	p := mathgraphEraRestartCertificate{
		Version: 1,
		Genesis: mathgraphPublishedSepoliaGenesis,
		ArchiveSHA256: common.HexToHash("0x" + mathgraphPublishedEra0SHA256),
		First: 0,
		Count: uint64(len(sources)),
		Tip: sources[len(sources)-1].block.Hash(),
	}
	return rlp.EncodeToBytes(&p)
}

func mathgraphCheckPublishedArchiveAt(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("archive unavailable: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != mathgraphPublishedEra0SHA256 {
		return fmt.Errorf("archive digest differs from publisher reference: %s", got)
	}
	return nil
}

func mathgraphStoredPrefixBinding(db ethdb.Database, expected []byte) (bool, error) {
	present, err := db.Has(mathgraphEraBindingKey)
	if err != nil {
		return false, err
	}
	if !present {
		return false, nil
	}
	stored, err := db.Get(mathgraphEraBindingKey)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(stored, expected) {
		return true, errors.New("persisted ERA binding differs from verified archive")
	}
	return true, nil
}

// A source may only be attached when its SHA, genesis, linked headers and all
// block commitments pass. The certificate must be present, and the persisted
// head must agree exactly; this never fills in missing history silently.
func mathgraphGuardedEraReattach(db ethdb.Database, sources []mathgraphEraSource, archivePath string) (*mathgraphGenesisPrefixView, error) {
	if err := mathgraphCheckPublishedArchiveAt(archivePath); err != nil {
		return nil, err
	}
	view, err := mathgraphOpenVerifiedGenesisPrefix(db, sources)
	if err != nil {
		return nil, err
	}
	cert, err := mathgraphExpectedRestartCertificate(sources)
	if err != nil {
		return nil, err
	}
	found, err := mathgraphStoredPrefixBinding(db, cert)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("verified ERA binding absent at restart")
	}
	tip := sources[len(sources)-1].block.Hash()
	if rawdb.ReadHeadHeaderHash(db) != tip || rawdb.ReadHeadFastBlockHash(db) != tip {
		return nil, errors.New("ERA binding present but native header indexing incomplete or inconsistent")
	}
	for i, src := range sources {
		n, ok := rawdb.ReadHeaderNumber(db, src.block.Hash())
		if !ok || n != uint64(i) {
			return nil, fmt.Errorf("ERA metadata incomplete or conflicting at %d", i)
		}
	}
	return view, nil
}

// Bootstrap writes the SMALL certificate before Geth's EXISTING index path.
// A crash between them can be recovered by rerunning this function with the
// verified source. A previously populated head with no matching certificate
// is refused, rather than silently interpreted as canonical.
func mathgraphGuardedEraBootstrap(db ethdb.Database, sources []mathgraphEraSource, archivePath string) (*mathgraphGenesisPrefixView, error) {
	if err := mathgraphCheckPublishedArchiveAt(archivePath); err != nil {
		return nil, err
	}
	view, err := mathgraphOpenVerifiedGenesisPrefix(db, sources)
	if err != nil {
		return nil, err
	}
	cert, err := mathgraphExpectedRestartCertificate(sources)
	if err != nil {
		return nil, err
	}
	found, err := mathgraphStoredPrefixBinding(db, cert)
	if err != nil {
		return nil, err
	}
	tip := sources[len(sources)-1].block.Hash()
	head := rawdb.ReadHeadHeaderHash(db)
	fast := rawdb.ReadHeadFastBlockHash(db)
	if !found && (head != (common.Hash{}) || fast != (common.Hash{})) {
		return nil, errors.New("orphaned metadata lacks its archive certificate")
	}
	if head != (common.Hash{}) && head != tip {
		return nil, fmt.Errorf("header head conflicts with certificate: %s", head)
	}
	if fast != (common.Hash{}) && fast != tip {
		return nil, fmt.Errorf("fast head conflicts with certificate: %s", fast)
	}
	for i, src := range sources {
		if n, ok := rawdb.ReadHeaderNumber(db, src.block.Hash()); ok && n != uint64(i) {
			return nil, fmt.Errorf("existing hash-number map conflicts at %d", i)
		}
	}
	if !found {
		if err := db.Put(mathgraphEraBindingKey, cert); err != nil {
			return nil, err
		}
	}
	rawdb.InitDatabaseFromFreezer(view)
	return mathgraphGuardedEraReattach(db, sources, archivePath)
}

func TestMathGraphEraRestartCertificateAllowsGuardedReattachment(t *testing.T) {
	sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	db := mathgraphFreshEraDatabase(t)
	view, err := mathgraphGuardedEraBootstrap(db, sources, mathgraphEra0Path())
	if err != nil {
		t.Fatal(err)
	}
	if head, err := view.Ancients(); err != nil || head != mathgraphGenesisPrefixLength {
		t.Fatalf("unexpected verified prefix length %d err=%v", head, err)
	}
	cert, err := mathgraphExpectedRestartCertificate(sources)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.Get(mathgraphEraBindingKey)
	if err != nil || !bytes.Equal(got, cert) {
		t.Fatal("durable ERA archive binding missing after bootstrap")
	}
	if _, err := mathgraphGuardedEraReattach(db, sources, mathgraphEra0Path()); err != nil {
		t.Fatalf("correct archive cannot reattach on simulated restart: %v", err)
	}
	// Repeated bootstrap is safe and reuses existing native metadata, not
	// execution state. No historical body/receipt is copied to the backing DB.
	if _, err := mathgraphGuardedEraBootstrap(db, sources, mathgraphEra0Path()); err != nil {
		t.Fatalf("reentrant guarded bootstrap rejected verified source: %v", err)
	}
	for _, n := range []uint64{0, 31, mathgraphGenesisPrefixLength-1} {
		if rawdb.ReadCanonicalHash(db, n) != (common.Hash{}) ||
			len(rawdb.ReadCanonicalBodyRLP(db, n, nil)) != 0 ||
			len(rawdb.ReadCanonicalReceiptsRLP(db, n, nil)) != 0 {
			t.Fatalf("durable certificate imported archival data at height %d", n)
		}
	}
	if rawdb.ReadHeadBlockHash(db) != (common.Hash{}) {
		t.Fatal("metadata binding fabricated an execution head")
	}
	t.Logf("VERIFIED_ERA_DURABLE_ARCHIVE_BINDING bytes=%d first=0 count=%d no_body_import=true", len(cert), mathgraphGenesisPrefixLength)
}

func TestMathGraphEraRestartBindingRejectsMissingOrChangedArchive(t *testing.T) {
	sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	db := mathgraphFreshEraDatabase(t)
	if _, err := mathgraphGuardedEraBootstrap(db, sources, mathgraphEra0Path()); err != nil {
		t.Fatal(err)
	}
	tip := sources[len(sources)-1].block.Hash()
	orig, err := db.Get(mathgraphEraBindingKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("ArchiveMissing", func(t *testing.T) {
		_, err := mathgraphGuardedEraReattach(db, sources, filepath.Join(t.TempDir(), "missing.era1"))
		if err == nil {
			t.Fatal("orphaned metadata was trusted without historical file")
		}
	})
	t.Run("ArchiveDigestMismatch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tampered.era1")
		if err := os.WriteFile(path, []byte("unverified ERA1 archive"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := mathgraphGuardedEraReattach(db, sources, path); err == nil {
			t.Fatal("unverified archived bytes became authoritative")
		}
	})
	t.Run("CertificateCorrupt", func(t *testing.T) {
		if err := db.Put(mathgraphEraBindingKey, []byte{0x01}); err != nil {
			t.Fatal(err)
		}
		if _, err := mathgraphGuardedEraReattach(db, sources, mathgraphEra0Path()); err == nil {
			t.Fatal("mismatched durable certificate was accepted")
		}
		if err := db.Put(mathgraphEraBindingKey, orig); err != nil {
			t.Fatal(err)
		}
	})
	if got := rawdb.ReadHeadHeaderHash(db); got != tip {
		t.Fatal("rejected view changed native metadata head")
	}
	if _, err := mathgraphGuardedEraReattach(db, sources, mathgraphEra0Path()); err != nil {
		t.Fatalf("correct source not recovered after rejected restart cases: %v", err)
	}
	t.Log("REJECTED_ERA_RESTART_WITHOUT_MATCHING_ARCHIVE_OR_CERTIFICATE")
}

func TestMathGraphEraRestartBindingDetectsOrphanedMetadata(t *testing.T) {
	sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	db := mathgraphFreshEraDatabase(t)
	// The previous unguarded qualified prototype is deliberately used as
	// a negative control: its metadata has no persistent source binding.
	view, err := mathgraphOpenVerifiedGenesisPrefix(db, sources)
	if err != nil {
		t.Fatal(err)
	}
	rawdb.InitDatabaseFromFreezer(view)
	if _, err := mathgraphGuardedEraBootstrap(db, sources, mathgraphEra0Path()); err == nil {
		t.Fatal("guard ignored existing head metadata without provenance")
	}
	if present, _ := db.Has(mathgraphEraBindingKey); present {
		t.Fatal("failed bootstrap invented certificate for orphan metadata")
	}
	t.Log("REJECTED_UNGUARDED_ERA_HEAD_WITHOUT_PROVENANCE")
}

func TestMathGraphEraRestartBindingRecoversCertificateOnlyCrash(t *testing.T) {
	sources := mathgraphLoadGenesisPrefix(t, mathgraphGenesisPrefixLength)
	db := mathgraphFreshEraDatabase(t)
	cert, err := mathgraphExpectedRestartCertificate(sources)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after persisting the certificate but before writing
	// any native index entries.
	if err := db.Put(mathgraphEraBindingKey, cert); err != nil {
		t.Fatal(err)
	}
	if rawdb.ReadHeadHeaderHash(db) != (common.Hash{}) {
		t.Fatal("crash fixture unexpectedly has an initialized head")
	}
	if _, err := mathgraphGuardedEraReattach(db, sources, mathgraphEra0Path()); err == nil {
		t.Fatal("partial initialization was accepted as a complete boot")
	}
	if _, err := mathgraphGuardedEraBootstrap(db, sources, mathgraphEra0Path()); err != nil {
		t.Fatalf("certificate-only interrupted bootstrap could not resume: %v", err)
	}
	if got := rawdb.ReadHeadHeaderHash(db); got != sources[len(sources)-1].block.Hash() {
		t.Fatalf("restarted bootstrap selected incorrect head %s", got)
	}
	t.Log("VERIFIED_ERA_CERTIFICATE_ONLY_RESTART_RECOVERY")
}
