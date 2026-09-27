# nats-server — static-analysis triage (`nix` branch)

Generated from the comprehensive tier (`nix run .#lint-comprehensive`) and gosec,
run **uncapped** (`--max-issues-per-linter 0 --max-same-issues 0`) over the whole tree.
Companion to [`quality-report.md`](./quality-report.md).

## Reality check first

nats-server is a mature, already well-linted codebase. **The gating set is 100% clean**
— `go vet`, the repo's own `.golangci.yml`, and all the Nix linters pass. Everything below
comes from the *advisory* comprehensive tier, which deliberately turns on opinionated linters
that upstream CI does not gate on.

Consequence for PR strategy: there are **no slam-dunk security bugs** hiding here. Every
gosec HIGH item is a known, deliberate pattern (see Tier 1). The value for warming up reviewers
is almost entirely in **Part B (easy wins)** — objectively-correct, behaviour-preserving,
one-line changes a maintainer approves without debate. Part A is criticality-ordered for
completeness, but most of its top entries are "won't fix / working as intended," and each is
labelled with an honest disposition so you don't file a PR that gets slapped down.

Uncapped totals: revive 1257 · gosec(golangci, unfiltered) 950 · gocritic 335 · gocyclo 200 ·
prealloc 71 · unconvert 62. Curated gosec (with `-exclude=G104,G115,G404 -exclude-dir=vendor`): **81**.

---

## Part A — findings ranked by criticality

Disposition legend: 🟥 worth a real look · 🟨 minor/defensible hardening · ⬜ deliberate / false-positive (don't file).

### Tier 1 — security (gosec), highest severity first

| # | Rule | Loc | What | Disposition |
|--:|------|-----|------|-------------|
| 1 | G407 (HIGH/HIGH) | `server/filestore.go:937,942` | "hardcoded IV/nonce" in JetStream block encryption | ⬜ taint FP — the nonce is generated, not hardcoded; append/`make` confuses the analyzer |
| 2 | G703 (HIGH/HIGH) | `server/jetstream.go:1598` | path traversal via taint on `os.WriteFile(keyFile,…)` | ⬜ `keyFile` is server-derived, not user input |
| 3 | G108 (HIGH/HIGH) | `server/server.go:44` | pprof endpoint auto-exposed via `_ "net/http/pprof"` | ⬜ deliberate; profiling is gated behind config, this is the documented mechanism |
| 4 | G109 (HIGH/MED) ×4 | `server/consumer.go:1236`, `server/subject_transform.go:280,297,367` | `int(strconv.Atoi(...))` overflow | 🟨 real-but-bounded; values are small config/partition ints. Cheap to add explicit bounds/`ParseInt` with bitSize |
| 5 | G122 (HIGH/MED) ×4 | `server/dirstore.go:180,246,290,360` | TOCTOU in `filepath.WalkDir` callback | ⬜ low-risk internal store walk; refactor cost > benefit |
| 6 | G402 (HIGH/LOW) | `server/opts.go:5833` | `InsecureSkipVerify` may be true | ⬜ it's a documented TLS config knob |
| 7 | G505 + G401 | `server/websocket.go:20,113` | crypto/sha1 import + weak-primitive use | ⬜ **required by RFC 6455** for `Sec-WebSocket-Accept`; not security-sensitive |
| 8 | G110 (MED/MED) | `server/stream.go:9555` | decompression bomb | 🟨 worth confirming the reader is size-bounded |
| 9 | G306 (MED/HIGH) | `server/server.go:4367` | `WriteFile` perms > 0600 | 🟨 legit tiny hardening candidate if the file isn't meant to be world-readable |
| 10 | G117 (MED/MED) ×6 | (various) | struct field `Pass` marshaled to JSON key `pass` | ⬜ intentional API/config field |
| 11 | G705 (MED/HIGH) ×3, G104/G703 taint | (various) | XSS/taint via analysis | ⬜ server-internal strings, not web-facing user input |

**Bottom line on Tier 1:** none of these are worth a security PR upstream. The two mild
hardening candidates are G109 bounds (#4) and G306 file perms (#9) — file those as
"[IMPROVED]" not "[FIXED]", and expect discussion.

### Tier 2 — correctness candidates (gocritic)

- 🟥 **`server/stree/stree.go:498,510`** — `append(pre, …)` inside the subject-tree traversal.
  Classic aliasing footgun: if `pre` ever has spare capacity, sibling iterations clobber each
  other's `subj`. **Worth a careful read** of how `pre` is grown up the tree; if it's always
  exact-cap this is safe, but it's the one finding here that could be a genuine latent bug.
- ⬜ The other 21 `appendAssign` hits are the intentional slice-delete idiom
  (`fs.blks = copyMsgBlocks(append(fs.blks[:i], fs.blks[i+1:]...))` at `filestore.go:11514`)
  or scratch-buffer building (`jetstream_batching.go:268,354`, `consumer.go:5524`). Don't file.
- ⬜ `badCall: suspicious Join on 1 argument` — both in `_test.go` only.

### Tier 3 — complexity hotspots (gocyclo, informational only)

Not fixes — refactoring these is large, risky, and reads as "your function is a mess." Listed
so you know where the density is, **not** as PR candidates:

`updateAccountClaimsWithRefresh` (139), `parseTLS` (94), `processServiceImport` (93),
`addConsumerWithAssignmentAndMode` (91), `EnableJetStream` (90), `updateWithAdvisory` (85),
`processJetStreamAtomicBatchMsg` (85), `fileStore.NumPending` (83), `RestoreStreamV2` (83),
`monitorCluster` (82). 200 functions total exceed complexity 30.

### Tier 4 — performance (prealloc)

Only **4** in production code (67 of 71 are in tests). Micro-optimisations; file only if the
slice is on a hot path, and expect "not hot, skip" on the rest.

---

## Part B — top 20 easy wins (the reviewer-warming set)

Every item below is objectively correct, behaviour-preserving, and mechanically checkable —
the kind of change a maintainer approves with "oh nice, thanks" and no ego friction. **Group
them into small, single-linter PRs** (reviewers approve focused PRs fastest). Suggested batching:

### PR 1 — remove unnecessary type conversions (`unconvert`) — 21 prod sites
Objectively dead conversions the compiler proves redundant. Zero behaviour change. The single
safest, most welcome PR in the set. (41 more in tests — separate PR if wanted.)

### PR 2 — use compound assignment operators (`gocritic assignOp`) — 10 prod sites
1. `server/accounts.go:2726` — `prefix = prefix + string(btsep)` → `prefix += …`
2. `server/filestore.go:1813,1826,6530,7781,7907,8591,11228` — `seq = seq &^ bit` → `seq &^= bit`
3. `server/filestore.go:6759` — `waited = waited + ts` → `waited += ts`
4. `server/filestore.go:14652` — `flags = flags | os.O_SYNC` → `flags |= os.O_SYNC`

### PR 3 — `strings.ReplaceAll` and friends (`gocritic wrapperFunc`) — 2 prod sites
5. `server/auth.go:591`
6. `server/auth.go:617`
(both `strings.Replace(s, a, b, -1)` → `strings.ReplaceAll(s, a, b)`)

### PR 4 — drop redundant full slices (`gocritic unslice`) — 7 prod sites
7. `server/consumer.go:2962`  8. `server/filestore.go:937`  9. `server/leafnode.go:1418`
10. `server/mqtt.go:652`  11. `server/raft.go:2073`  12. `server/server.go:3415`
13. `server/server.go:3446`  (all `x[:]` → `x`)

### PR 5 — clearer length checks (`gocritic sloppyLen`) — prod sites
14. `server/ocsp.go:398` — `len(cert.Certificate) <= 0` → `== 0`
15. `server/proto.go:85` — `len(b) <= 0` → `== 0`
(the rest are in tests)

### PR 6 — comment formatting (`gocritic commentFormatting`) — 6 prod sites
16–20. Add the space in `//comment` → `// comment` (6 in production code, 23 in tests). Trivial,
gofmt-adjacent, uncontroversial. Good "grouped tidy-up" PR.

**Also strong, if you want more (revive, same safety class):**
- `increment-decrement` (28): `i += 1` → `i++`
- `empty-block` (25): remove/annotate empty blocks
- `superfluous-else` / `indent-error-flow` (112 combined): early-return style — usually welcome
  but *can* bikeshed; keep these to one small PR and gauge reception before doing more.

### What to avoid for warm-up PRs
`revive var-naming` (393) and `redefines-builtin-id` (117) touch identifiers/renames — high
bikeshed + ego risk. `ifElseChain` (197, if→switch) is a large opinionated diff. `gocyclo`
refactors imply criticism. Save these until you've built goodwill.

---

## Reproduce

```
nix run .#quality-report                 # aggregated markdown (all tools)
nix run .#lint-comprehensive             # full comprehensive tier to stdout
nix run .#gosec                          # curated gosec (81 findings)
nix run .#govulncheck                    # CVE scan (needs network)
```
Uncapped JSON used for this triage:
`golangci-lint run --config .golangci-comprehensive.yml --max-issues-per-linter 0 --max-same-issues 0 --output.json.path <f> ./...`
