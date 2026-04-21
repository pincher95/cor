# `cmd/` Cleanup Sweep — Design

**Status:** Design approved 2026-04-20. Implementation plan to follow.

Follow-up to refactors A (shell runner) and B (pipeline abstraction). Bundles three small-to-medium cleanups that landed as deferred items during those reviews. Each phase is independently correct — the codebase is shippable after any phase completes.

## Motivation

Refactors A and B each deferred a cluster of small improvements to keep their own scopes tight. Those items are now visible enough in the reviewers' notes that they're worth landing before the next feature work ("C": new orphan-resource commands, cross-cutting features like JSON output). This sweep knocks them out in one coordinated spec:

1. **helpers.go consolidation** — ENIs' `splitCSV`/`mergeCSV`/`getFlagString` utilities and its cache type were hoisted from closures to package-level during refactor B. They work correctly but live in the wrong file. Candidate for collision with future commands that need CSV filtering.
2. **`Found N orphaned X` summary log** — refactor B retired this log uniformly across the 11 converted commands. The retirement was never explicitly decided; it was a consequence of the `runOrphanPipeline` helper not knowing what resource label to use. Add a `ResourceLabel` field so the helper can restore this operator-useful log.
3. **Typed `GlobalFlags` struct** — the biggest deferred item. Every command reads the 6 global flags (region, profile, auth-method, delete, sort-by, sort-desc) via `(*flagValues)["sort-by"].(string)` and similar. The type-assertion surface is large and panic-prone if `flags.GetFlags` ever emits a wrong type. A typed struct eliminates the class entirely for globals.

None of these is urgent on its own; bundling avoids three separate design-plan-execute cycles.

## Scope

### In scope

- **Phase 1 — helpers.go consolidation:**
  - Move `splitCSV`, `mergeCSV`, `getFlagString` from `cmd/enis.go` to `cmd/helpers.go`. Signatures unchanged.
  - Rename `eniNameCache` → `awsNameCache` and move to `cmd/helpers.go`. Same 3-map shape (`vpcs`, `subnets`, `sgs`) + `sync.RWMutex`. The 3 methods that use it (`getVPCName`, `getSubnetName`, `ensureSGNames`) stay on `*AWSCommand` in `cmd/enis.go` — they are ENI-specific enrichment wrappers even if the cache shape is generic.

- **Phase 2 — `OrphanPipeline.ResourceLabel`:**
  - Add `ResourceLabel string` field to `OrphanPipeline[Item, Result]` in `cmd/pipeline.go`.
  - In `runOrphanPipeline`, emit `a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned %s", len(collected), spec.ResourceLabel), nil)` after the collector drains and before the delete phase. Emit only when `ResourceLabel != ""` (empty = opt out; preserves current no-log behavior for future commands that don't want it).
  - Update the 11 converted commands' spec literals with labels matching pre-refactor wording.

- **Phase 3 — `GlobalFlags` typed struct:**
  - Introduce `flags.GlobalFlags{Region, Profile, AuthMethod string; Delete bool; SortBy string; SortDesc bool}` in `pkg/handlers/flags/flags.go`.
  - Change `flags.GetFlags` signature from `(r, extras) (*map[string]any, error)` → `(r, extras) (*GlobalFlags, *map[string]any, error)`. The returned extras map holds only per-command keys; the 6 global keys are moved into the struct and no longer appear in the map.
  - Update `runResourceCommand` (in `cmd/runner.go`) to pass `*GlobalFlags` + `*map[string]any` to the execute callback.
  - Update `runOrphanPipeline` (in `cmd/pipeline.go`) signature to take `*GlobalFlags` + `*map[string]any` + spec. Reads globals via struct (`.Delete`, `.SortBy`, `.SortDesc`).
  - Update all 25 `executeX` methods to the new signature. Read global flags via the struct; per-command flags unchanged (still read from extras map).

### Out of scope

- **Per-command typed flag structs.** Each command could define its own typed struct for per-command flags (Option β from brainstorming). Deferred — each executeX touches the extras map at most a few times, and the blast radius of a typo is one command. The cost/benefit doesn't justify another 25-file sweep.
- **Error-surface improvements to `GetFlags`.** The function currently never errors in practice (flag retrieval is guaranteed by cobra registration); we preserve that.
- **Flag registration changes.** `rootCmd.PersistentFlags().StringP(...)` in `cmd/root.go` and per-command `init()` blocks stay unchanged. Only the flag *retrieval* side moves to typed values.
- **Any new logging beyond Phase 2's one line per command.** No `LogInfo("Deleting X")` consistency pass, no log-format changes.

## Phase 1 — helpers.go consolidation

### Motivation

`cmd/enis.go` became the only file containing CSV-handling helpers during refactor B. The helpers are generic enough to be reused: if Task 5 or Task 6 had needed CSV filtering (they didn't), the next author would have either re-implemented or grep-found the existing helpers and had to decide whether to relocate. Relocating now, before any reuse, avoids an "oh wait, this lives over there" moment.

`eniNameCache`'s structure (three string→string maps + a mutex) is identical to what any VPC/subnet/SG-adjacent command might want. Renaming before reuse is cheap.

### Changes

Move to `cmd/helpers.go`:

```go
// awsNameCache is a goroutine-safe cache of AWS resource-name lookups
// keyed by VPC ID, subnet ID, and security-group ID. It eliminates
// duplicate Describe* calls when many items share the same VPC/subnet/SG.
type awsNameCache struct {
    mu      sync.RWMutex
    vpcs    map[string]string
    subnets map[string]string
    sgs     map[string]string
}

func newAWSNameCache() *awsNameCache {
    return &awsNameCache{
        vpcs:    make(map[string]string),
        subnets: make(map[string]string),
        sgs:     make(map[string]string),
    }
}

// splitCSV parses a comma-separated filter string into a slice of trimmed,
// non-empty values. "*" or "" yields nil (meaning "no filter").
func splitCSV(s string) []string { /* unchanged body from enis.go */ }

// mergeCSV joins two CSV strings, deduplicating entries while preserving
// first-seen order. Used to merge a primary flag with its backwards-compat
// alias.
func mergeCSV(a, b string) string { /* unchanged */ }

// getFlagString reads a string value from the per-command extras map,
// returning "" if the key is absent or not a string.
func getFlagString(extras *map[string]any, name string) string { /* unchanged */ }
```

In `cmd/enis.go`:

- Delete the old definitions of `splitCSV`, `mergeCSV`, `getFlagString`, `eniNameCache`, `newENINameCache`.
- Update all usages: `*eniNameCache` → `*awsNameCache`, `newENINameCache()` → `newAWSNameCache()`.
- The three methods (`getVPCName`, `getSubnetName`, `ensureSGNames`) stay on `*AWSCommand` and stay in `cmd/enis.go` but their cache param type updates to `*awsNameCache`.

No behavior change.

## Phase 2 — `ResourceLabel` summary log

### Change to `cmd/pipeline.go`

Add one field to `OrphanPipeline`:

```go
type OrphanPipeline[Item, Result any] struct {
    Headers       []string
    // ResourceLabel is the plural human-readable name of the resource type
    // (e.g. "Lambda functions", "ENIs"). When non-empty, runOrphanPipeline
    // emits "Found %d orphaned <ResourceLabel>" via Logger.LogInfo after
    // streaming all rows and before the delete phase.
    ResourceLabel string
    HideIndex     bool
    List          func(ctx context.Context, emit func(Item) error) error
    Process       func(ctx context.Context, item Item) (*Result, error)
    ToRow         func(r Result) []any
    Finalize      func(results []Result) []any
    Delete        func(ctx context.Context, r Result) error
}
```

Add one block to `runOrphanPipeline`, inserted between `<-collectorDone` and the `if !collectDeletes || len(collected) == 0 { return nil }` check:

```go
if spec.ResourceLabel != "" {
    a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned %s", len(collected), spec.ResourceLabel), nil)
}
```

Add `"fmt"` to `cmd/pipeline.go` imports.

### Labels for the 11 converted commands

Match pre-refactor wording verbatim:

| Command | `ResourceLabel` | Pre-refactor log text |
|---|---|---|
| volumes | `"EBS volumes"` | `"Found N orphaned EBS volumes"` |
| enis | `"ENIs"` | `"Found N orphaned ENIs"` |
| elasticaddresses | `"Elastic IPs"` | `"Found N orphaned Elastic IPs"` |
| lambda | `"Lambda functions"` | `"Found N orphaned Lambda functions"` |
| elasticache | `"ElastiCache clusters"` | `"Found N orphaned ElastiCache clusters"` |
| opensearch | `"OpenSearch domains"` | `"Found N orphaned OpenSearch domains"` |
| s3buckets | `"S3 buckets"` | `"Found N orphaned S3 buckets"` |
| dynamodb | `"DynamoDB tables"` | `"Found N orphaned DynamoDB tables"` |
| targetgroups | `"target groups"` | `"Found N orphaned target groups"` |
| natgateways | `"NAT Gateways"` | `"Found N orphaned NAT Gateways"` |
| ecs | `"ECS clusters"` | `"Found N orphaned ECS clusters"` |

Where the pre-refactor command didn't have this exact log (because the implementer at the time chose different wording or omitted it), use the shape that reads naturally.

Always emits, including when `len(collected) == 0` — matches the original pre-refactor semantics across all commands that had the log.

## Phase 3 — `GlobalFlags` typed struct

### `pkg/handlers/flags/flags.go` — the type

```go
// GlobalFlags holds the six root-level flags that every command shares.
// Commands read them directly from the struct; the extras map returned
// alongside holds only per-command flags.
type GlobalFlags struct {
    Region     string
    Profile    string
    AuthMethod string
    Delete     bool
    SortBy     string
    SortDesc   bool
}
```

### `pkg/handlers/flags/flags.go` — new `GetFlags` signature

```go
// Before:
func GetFlags(flagRetriever FlagRetriever, additionalFlags []Flag) (*map[string]any, error)

// After:
func GetFlags(flagRetriever FlagRetriever, additionalFlags []Flag) (*GlobalFlags, *map[string]any, error)
```

Implementation: the function already iterates a `baseFlags` slice then `additionalFlags`. Split the loop so base flags populate the `GlobalFlags` struct fields directly (no type assertion at call sites) and additional flags populate the extras map with `any` typing. The CLI > config > env > default precedence logic (via Viper) is unchanged — only the output shape differs.

### `cmd/runner.go` — runner signature

```go
// Before:
func runResourceCommand(
    cmd *cobra.Command,
    setup CommandSetup,
    execute func(awsCmd *AWSCommand, ctx context.Context, flagValues *map[string]any) error,
) error

// After:
func runResourceCommand(
    cmd *cobra.Command,
    setup CommandSetup,
    execute func(awsCmd *AWSCommand, ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error,
) error
```

The internal `CloudConfig` construction inside `runResourceCommand` (currently reads `(*flagValues)["region"].(string)` etc.) becomes direct struct-field reads.

### `cmd/pipeline.go` — pipeline signature

```go
// Before:
func runOrphanPipeline[Item, Result any](
    a *AWSCommand,
    ctx context.Context,
    flagValues *map[string]any,
    spec OrphanPipeline[Item, Result],
) error

// After:
func runOrphanPipeline[Item, Result any](
    a *AWSCommand,
    ctx context.Context,
    globals *flags.GlobalFlags,
    extras *map[string]any,
    spec OrphanPipeline[Item, Result],
) error
```

Body: `collectDeletes := globals.Delete && spec.Delete != nil`, `stream.SetSort(globals.SortBy, globals.SortDesc)`. No map reads for these.

### 25 `executeX` method signatures

Every command's `executeX` method changes from:

```go
func (v *AWSCommand) executeVolumes(ctx context.Context, flagValues *map[string]any) error
```

to:

```go
func (v *AWSCommand) executeVolumes(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error
```

Inside the method body:

- Global-flag reads like `(*flagValues)["auth-method"].(string)` → removed entirely (the runner already built `CloudConfig` from typed fields; commands don't see auth-method, profile, or region directly).
- `(*flagValues)["delete"].(bool)` → `globals.Delete`
- Similarly for sort-by, sort-desc.
- Per-command reads like `(*flagValues)["filter-by-name"].(string)` → `(*extras)["filter-by-name"].(string)` (just rename `flagValues` → `extras`).

Pipeline-using commands pass `globals` and `extras` through to `runOrphanPipeline`:

```go
return runOrphanPipeline(v, ctx, globals, extras, OrphanPipeline[...]{...})
```

### Transition strategy

Because Phase 3 changes a signature used by every command, the task order matters:

1. Add `GlobalFlags` type, rewrite `GetFlags`, update runner + pipeline helpers all in one task. This breaks the 25 existing `executeX` methods temporarily.
2. Immediately convert all 25 methods in batches of 10–12. Each batch is mechanical; no behavior change.
3. Run the whole suite and pre-commit on each batch.

The compile errors between step 1 and step 2 are useful — they enforce that every command gets updated. No hidden fallthrough paths.

Alternative considered: add a backward-compat adapter layer. Rejected because (a) maintaining the adapter is extra work, (b) no branch of the codebase needs to stay on the old shape, and (c) the compile-error fence is a feature, not a bug.

## Migration strategy

Seven tasks.

1. **Task 1 — Phase 1:** move `splitCSV`/`mergeCSV`/`getFlagString` + `awsNameCache` (renamed from `eniNameCache`) to `cmd/helpers.go`. Update `cmd/enis.go` references. Build, vet, test, race, pre-commit, commit.

2. **Task 2 — Phase 2:** add `ResourceLabel` field to `OrphanPipeline`, update `runOrphanPipeline` to emit the summary log, update 11 spec literals. Update `TestRunOrphanPipeline_HappyPath_StreamsAllRows` to assert the summary log appears when `ResourceLabel` is set. Build, vet, test, race, pre-commit, commit.

3. **Task 3 — Phase 3 prep:** add `GlobalFlags` type to `pkg/handlers/flags`. Change `GetFlags` signature. Update `runResourceCommand` and `runOrphanPipeline` signatures. Update existing tests that touch these signatures: the `TestGetFlags_*` suite in `pkg/handlers/flags/flags_test.go`, `TestRunResourceCommand_HappyPath` in `cmd/runner_test.go`, and all 8 `TestRunOrphanPipeline_*` in `cmd/pipeline_test.go`. Add a new test `TestGetFlags_ReturnsTypedGlobals`.

   This task intentionally breaks the 25 `executeX` methods — they won't compile until Task 4+. That's the compile-fence discipline.

4. **Task 4 — Phase 3 pilot:** convert `cmd/volumes.go`'s `executeVolumes` signature + body. One command. Walking skeleton. Commit.

5. **Task 5 — Phase 3 batch 1:** convert 12 more commands (autoscaling, clientvpn, dynamodb, ecr, ecs, efs, elasticache, elasticaddresses, elbv1, elbv2, enis, images).

6. **Task 6 — Phase 3 batch 2:** convert the remaining 12 (lambda, logs, natgateways, opensearch, rds, route53zones, s3buckets, snapshots, targetgroups, tgwattachments, vpcendpoints, vpnconnections). After this batch, `go build ./...` is clean again.

7. **Task 7 — final verification:** `grep -r "(\*flagValues)" cmd/` expects zero hits. `grep -r "(\*extras)" cmd/` counts the per-command flag reads. Full build/vet/test/race/pre-commit. LOC comparison vs the pre-sweep baseline.

Expected LOC delta: slightly positive — `GlobalFlags` adds ~10 lines of type declaration plus 6 lines of field population in `GetFlags`, offset by removing ~6 map-key string literals per `executeX` call site (×25 commands). Probably a wash or small negative net.

## Testing

- **Phase 1:** no test changes. The moved helpers are exercised indirectly by enis and (once moved) available to other tests.
- **Phase 2:** extend `TestRunOrphanPipeline_HappyPath_StreamsAllRows` (in `cmd/pipeline_test.go`) to pass `ResourceLabel: "test items"` and assert the output contains `"Found 3 orphaned test items"`. Keep all 8 existing assertions. Still 8 tests.
- **Phase 3:**
  - Add `TestGetFlags_ReturnsTypedGlobals` in `pkg/handlers/flags/flags_test.go` — fake cobra.Command with the 6 globals + one additional flag; assert the returned `*GlobalFlags` has correct field values AND the extras map contains only the one additional flag (not the 6 globals).
  - Update `TestGetFlags` existing cases to the new signature (3-return-value form).
  - Update `TestRunResourceCommand_HappyPath` in `cmd/runner_test.go` to assert the execute callback receives a non-nil `*GlobalFlags` with expected values, plus an extras map containing the additional flags.
  - Update all `TestRunOrphanPipeline_*` (8 tests) to the new signature. The happy-path tests pass a fake `*GlobalFlags` directly; the error-path tests unchanged in semantics.

## Risks

- **Phase 3 is a wide signature change.** 25 `executeX` methods + 2 helpers + their tests. If the compile-fence approach surfaces a file I didn't anticipate (e.g. an out-of-scope command whose `executeX` doesn't go through the runner at all), that file fails to build and the batch needs to include it. Mitigation: audit before starting Task 3 that every `func (x *AWSCommand) execute` method has the current signature.
- **`getFlagString`'s signature stability across the move.** If Phase 3 also renames `flagValues` → `extras` at call sites, `getFlagString` needs its single parameter name/type kept stable in Phase 1 (so Phase 1 doesn't conflict with Phase 3). Already the case — Phase 1 moves without renaming the parameter.
- **Log timing for Phase 2.** The `"Found N orphaned X"` log appears after all rows stream. Streaming uses `StreamTable`, which can flush asynchronously. Need to ensure the log appears after the last row is visible to the user. Mitigation: the current `<-collectorDone` barrier already guarantees the stream is closed before the log line emits. No sequencing issue.
- **Per-command label casing divergence.** The table above uses sentence-case plurals except "target groups" (lower-case, matches old wording). If any pre-refactor log used different casing than the table suggests, preserve the exact pre-refactor wording rather than normalizing.

## Definition of done

After Task 7:

- `cmd/helpers.go` contains `splitCSV`, `mergeCSV`, `getFlagString`, and `awsNameCache`. `cmd/enis.go` no longer defines them.
- `OrphanPipeline.ResourceLabel` exists. All 11 converted commands populate it with the correct label. Running `go run . <any-of-11> --delete` shows the "Found N" summary line.
- `flags.GlobalFlags` exists. `flags.GetFlags` returns `(*GlobalFlags, *map[string]any, error)`.
- Zero hits of `(*flagValues)` in `cmd/*.go` (the old map-cast pattern is gone).
- `go build/vet/test/race/pre-commit` all clean.
- No user-visible behavior change beyond the restored summary log.
