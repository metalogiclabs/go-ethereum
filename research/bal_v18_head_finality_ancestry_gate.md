# V18 — Finalized hash must belong to the announced Engine head

Status: CANDIDATE. This file is a preregistered falsifier, not a passing test or a claim of safety.

## Authority
Parent V17 source: 148b803f56fcfb541c1d3de54701f745d05fe604.
Parent qualification: https://github.com/metalogiclabs/go-ethereum/actions/runs/38199150470
Parent artifact: 11723968809. Pinned official Geth ancestor: 3a6c2e3b35ef8caa3d39dcbea3fe9f09099cca8c.
No upstream change or production deployment.

## Residual
The experimental wrapper checks that a finalized header matches the JWT-authenticated FCU finalized hash, but not that this finalized header belongs to the ancestry of the requested FCU head. JWT authenticates the caller; it does not certify consistency of arbitrary submitted head/finality pairs.

## Smallest decisive test
Build two valid chains A and B with a common genesis and divergent headers. Supply a cold, unknown announced head on A and a retrievable finalized hash from B through the same authenticated engine_forkchoiceUpdatedV4(nil attributes) call. Require the wrong-branch finalized witness to be denied BEFORE admission, independently of any later block-state root check. Repeat with reversed branches. Positive control: finalized header is an ancestor of A's announced head and V17's 65/65 executionless test still passes. Missing intermediate ancestry and unavailable head must fail closed. Keep V15 revoke/ABA and V17 malformed-attributes controls.

## Necessary architecture
Resolve the requested head header and the claimed finalized header. Walk exact parent hashes from head down to finalized height, with bounded work and an authenticated header source. Admit only if the hash at finalized height matches exactly. Missing/ambiguous headers, mismatched heights, inconsistent ancestry, zero hashes, or cancellation imply no pending authorization. Do not merely compare heights, hash validity, or a header independently retrieved from a peer. Avoid blocking the Engine RPC indefinitely; declare resource budgets. Admission must still carry V15 epoch/commit fence.

## Decision
Wrong-branch admission => REJECTED for current wrapper, preserve counterexample. Wrong-branch rejection + positive controls => WARRANTED_BOUNDED for this exact ancestry check, not universal CL/production safety. Concurrent FCUs, malicious peers, crash/reorg, independent Lighthouse/Prysm CL and mainnet performance remain UNKNOWN.

## Follow-on
Once this fork consistency gate passes, use a deterministic interleaving of two authenticated FCUs during header resolution, followed by crash/restart and independent CL devnet. Do not promote to upstream or merge until evidence passes.
