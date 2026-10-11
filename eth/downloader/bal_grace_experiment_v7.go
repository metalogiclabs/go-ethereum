//go:build balexperiment

// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Fork-only qualification accessors. These are absent from normal builds and
// are not a production configuration or an upstream patch proposal.
package downloader

import "time"

// ConfigureBALGraceForExperiment opts a single test downloader into the
// existing bounded-wait policy before starting the test synchronization.
func (d *Downloader) ConfigureBALGraceForExperiment(window time.Duration) {
	if window < 0 || window > 25*time.Millisecond {
		panic("invalid BAL experiment grace duration")
	}
	d.queue.balGraceWindow = window
	d.queue.balGraceRequireInFlight = true
}

// BALGraceConsumedForExperiment is read only after a test node has stopped.
// The downloader's Results loop is then no longer mutating its queue.
func (d *Downloader) BALGraceConsumedForExperiment() time.Duration {
	return d.queue.balGraceSpent
}
