# MathGraph Check external native evidence — controlled App consumption fixture

This **draft PR exists only in the Metalogic-owned go-ethereum fork** to exercise the already installed [MathGraph Check GitHub App](https://github.com/organizations/metalogiclabs/settings/apps). It must not be merged into Ethereum, submitted upstream or treated as an Ethereum source-code change.

The attached `.mathgraph-check/external-native-evidence.v1.json` is a strictly pinned, *external* research receipt. Its real subject is [goccy/go-yaml PR #958](https://github.com/goccy/go-yaml/pull/958) and the preexisting independent [issue #872](https://github.com/goccy/go-yaml/issues/872). Source-native experiments were run in [heathsanchez/test](https://github.com/heathsanchez/test) with original Go tests, 130 independent nesting cases and a one-file causal ablation.

**Subject-correct maintainer decision:** 69/130 previously failing cases are repaired by the author's PR; no introduced regressions occurred within that matrix; 20 inherited block-chomping failures remain. The separately reviewed one-file repair resolves the 20 cases in native tests, and removal reproduces them. The right action is to review issue #872 separately—not block PR #958 for an inherited, explicitly out-of-scope problem.

The GitHub App must independently recheck exact public GitHub run and artifact metadata, plus the pinned report blob, before showing this *historical bounded evidence*. When metadata cannot be rechecked, mark the external receipt **UNKNOWN**, preserving the generic check.

**Do not misattribute:** This is an evidence-carrier PR and says nothing about the safety of this Ethereum fork. The App must retain `neutral`, `ADVISORY_ONLY` status and link its details to the native GitHub Check Run.

Research data are metadata and links, not secrets. No human reviewer is requested, no upstream issue or PR is modified, and no code from the target research repository is executed by the App.
