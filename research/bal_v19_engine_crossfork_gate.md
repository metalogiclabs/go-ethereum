# V19 — Authenticated Engine cross-fork finality test

Status: CANDIDATE; no test run yet. Parent V18: 7c48a0811a21dad7ff192171e905663966e15c95; green run 38201111385, artifact 11725227647.

## Objective
Move V18's exact DAG ancestry negative from direct helper calls to a real JWT-protected engine_forkchoiceUpdatedV4 call while preserving two genuine Geth TCP peers and persistent target DB.

## Experimental cases
1. Generate A and B chains with shared genesis and divergent headers. Serve A head and B finalized header via real peer(s). Ensure the target starts without either head locally. Send JWT Engine FCU(head=A_tip, finalized=B_ancestor, attrs=nil). Require no pending witness admission even when both individual hashes resolve.
2. Reverse A/B; same rejection.
3. Valid A_tip/A_ancestor pair; require pending witness admission and successful executionless import, identical state root, receipt partition, restart parity.
4. Missing intermediate header; fail closed without using unverified local height comparison.
5. Start FCU A, block its header fetch, then issue conflicting FCU B; stale A must not install authority after B supersedes it. Test epoch fence through durable commit.

## Admission criteria
Each negative must show actual authenticated RPC invocation and authenticated peer responses, zero pending witness hash/admissions, no executionless import attributable to wrong authority. Positive must retain V18 65/65 behavior. Independently seal exact source tree, logs, per-arm counters and test artifact. Preserve V15 ABA, V17 malformed FCU and V18 helper controls.

## Caveat
Geth's original Engine API may return SYNCING on unknown head, so a status of SYNCING alone does not certify an FCU's semantic correctness. A JWT credential proves caller authentication, not the relationship between the finalized hash and announced head. Do not admit based solely on a fetched finalized header.

No production or upstream changes. If test harness cannot construct the remote competing fork, report UNKNOWN rather than treating a direct helper unit test as an Engine integration result.
