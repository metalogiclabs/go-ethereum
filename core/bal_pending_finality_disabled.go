//go:build !balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Normal Geth has neither pending finality authority nor a commit
// synchronization capability. Tagged research code is not in this build.
package core

import "github.com/ethereum/go-ethereum/core/types"

type balPendingPermit struct{}

func (bc *BlockChain) pendingBALFinality() *types.Header {
	return nil
}

func (bc *BlockChain) pendingBALAdmissionPermit() balPendingPermit {
	return balPendingPermit{}
}

func (bc *BlockChain) pendingBALCommitFence(_ balPendingPermit) (func(), bool) {
	return func() {}, true
}

func (bc *BlockChain) experimentalPendingBALOnStop() {}

func (bc *BlockChain) experimentalBALAdmissionBarrier() {}
