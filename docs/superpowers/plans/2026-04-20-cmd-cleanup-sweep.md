# `cmd/` Cleanup Sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bundle three deferred cleanups from refactors A and B: relocate ENIs' CSV helpers + `awsNameCache` to `cmd/helpers.go`; restore the `"Found N orphaned X"` summary log via `OrphanPipeline.ResourceLabel`; introduce a typed `flags.GlobalFlags` struct so the six root flags stop being accessed via `(*flagValues)["x"].(string)` panic-prone map-casts.

**Architecture:** Three phases landed as four tasks. Phase 1 (one commit) moves shared utilities. Phase 2 (one commit) adds the summary-log field and populates it for the 11 pipeline commands. Phase 3 (one big atomic commit) flips `GetFlags`'s return shape and ripples the signature change through `runResourceCommand`, `runOrphanPipeline`, and every `executeX` in `cmd/`. Task 4 runs end-of-sweep audits.

**Tech Stack:** Go 1.26.2, Cobra + Viper, AWS SDK Go v2, stdlib testing.

**Spec:** `docs/superpowers/specs/2026-04-20-cmd-cleanup-sweep-design.md`.

---

## Prerequisites

Read `CLAUDE.md` at repo root for:
- Conventional-commits message format (type + scope required, header ≤120 chars, body/footer lines ≤100 chars).
- Pre-commit hooks: `go-fmt`, `go-vet`, `go-imports`, `golangci-lint`, `go-unit-tests`, `go-build`, `go-mod-tidy`.
- Do NOT use `git commit --no-verify` or `--amend`. Fix hook failures via a NEW commit.

Read the spec (link above) for design rationale. Highlights:
- `runOrphanPipeline` is a **package-level function**, not a method (Go forbids methods with type parameters). Call sites use `runOrphanPipeline(a, ctx, globals, extras, spec)` after Phase 3.
- `GlobalFlags` is typed; per-command flags still go through the `*map[string]any` extras map.
- 11 of the 25 commands already use `runOrphanPipeline`; the other 14 either use the sequential-pagination shape (autoscaling, clientvpn, efs, vpcendpoints, vpnconnections, tgwattachments, ecr, route53zones, snapshots, elbv1) or are out-of-scope pipeline-custom files (rds, logs, images, elbv2).

### File inventory (25 executeX commands)

Pipeline-using (11): `volumes, enis, elasticaddresses, lambda, elasticache, opensearch, s3buckets, dynamodb, targetgroups, natgateways, ecs`.
Sequential / out-of-scope for pipeline (14): `autoscaling, clientvpn, efs, vpcendpoints, vpnconnections, tgwattachments, ecr, route53zones, snapshots, elbv1, rds, logs, images, elbv2`.

All 25 will be touched in Phase 3.

### Scope reminder

- **In scope:** moving 3 helper functions + 1 cache type (Phase 1), adding 1 spec field + 1 log line + 11 spec-literal populations (Phase 2), typing 6 global flags + signature ripple across 28 files (Phase 3).
- **Out of scope:** per-command typed flag structs; any flag-registration changes; any new logs beyond the Phase 2 summary.

---

## Task 1 — Phase 1: `cmd/helpers.go` consolidation

**Files:**
- Modify: `cmd/helpers.go` (add helpers + cache type)
- Modify: `cmd/enis.go` (remove moved definitions; rename `eniNameCache` references to `awsNameCache`)

### Steps

- [ ] **Step 1: Read `cmd/enis.go` lines 44–52 and 443–489 to confirm current definitions**

Run: `sed -n '44,52p;443,489p' cmd/enis.go`

Expected output (verbatim) includes these blocks:
- `type eniNameCache struct { mu sync.RWMutex; vpcs, subnets, sgs map[string]string }` at lines 47–52
- `func splitCSV(raw string) []string { ... }` at lines 446–464
- `func getFlagString(flagValues *map[string]any, name string) string { ... }` at lines 467–472
- `func mergeCSV(flagValues *map[string]any, names ...string) []string { ... }` at lines 476–489

These are the exact bodies that need moving. No modification — move verbatim, then rename the type only.

- [ ] **Step 2: Append definitions to `cmd/helpers.go`**

Add to the **end** of `cmd/helpers.go` (after all existing functions):

```go
// awsNameCache is a goroutine-safe cache of AWS resource-name lookups
// keyed by VPC ID, subnet ID, and security-group ID. It eliminates
// duplicate Describe* calls when many items share the same VPC/subnet/SG.
//
// The three maps are independent. A caller that only populates one map
// (e.g. only subnet names) pays no cost on the other two.
type awsNameCache struct {
    mu      sync.RWMutex
    vpcs    map[string]string
    subnets map[string]string
    sgs     map[string]string
}

// splitCSV splits a comma-separated filter value into trimmed, non-empty
// tokens, skipping "*" (the "no filter" sentinel). Returns nil when the
// input is empty or contains only skipped tokens.
func splitCSV(raw string) []string {
    raw = normalizeFilterValue(raw)
    if raw == "" {
        return nil
    }
    parts := strings.Split(raw, ",")
    out := make([]string, 0, len(parts))
    for _, p := range parts {
        p = strings.TrimSpace(p)
        if p == "" || p == "*" {
            continue
        }
        out = append(out, p)
    }
    if len(out) == 0 {
        return nil
    }
    return out
}

// getFlagString returns the string value for the given flag name, or "".
func getFlagString(flagValues *map[string]any, name string) string {
    if v, ok := (*flagValues)[name].(string); ok {
        return v
    }
    return ""
}

// mergeCSV unions CSV tokens from the named flags, preserving first-seen
// order and dropping duplicates.
func mergeCSV(flagValues *map[string]any, names ...string) []string {
    seen := make(map[string]struct{}, 8)
    out := make([]string, 0)
    for _, n := range names {
        for _, v := range splitCSV(getFlagString(flagValues, n)) {
            if _, ok := seen[v]; ok {
                continue
            }
            seen[v] = struct{}{}
            out = append(out, v)
        }
    }
    return out
}
```

Also ensure `"sync"` is imported in `cmd/helpers.go` (it currently is not — the existing helpers don't need it). Add `"sync"` to the imports block (alphabetical order among stdlib imports).

- [ ] **Step 3: Remove the moved definitions from `cmd/enis.go`**

In `cmd/enis.go`, delete these blocks:
- Lines 44–52: the `eniNameCache` type + its doc comment.
- Lines 443–489: `splitCSV`, `getFlagString`, `mergeCSV` (and the doc comments above each).

- [ ] **Step 4: Rename `eniNameCache` → `awsNameCache` at all remaining call sites in `cmd/enis.go`**

Four references remain after Step 3:
- Line 124 (approx, after deletions shift numbers): `cache := &eniNameCache{...}` → `cache := &awsNameCache{...}`
- Line 333 (approx): `func (e *AWSCommand) getVPCName(ctx context.Context, cache *eniNameCache, vpcID string) string` → `*awsNameCache`
- Line 359 (approx): `func (e *AWSCommand) getSubnetName(ctx context.Context, cache *eniNameCache, subnetID string) string` → `*awsNameCache`
- Line 387 (approx): `func (e *AWSCommand) ensureSGNames(ctx context.Context, cache *eniNameCache, sgIDs []string)` → `*awsNameCache`
- Line 430 (approx): `func formatSGList(cache *eniNameCache, sgIDList []string) string` → `*awsNameCache`

Use a simple textual replace across `cmd/enis.go` — `eniNameCache` → `awsNameCache` (five occurrences). The pre-refactor `newENINameCache` helper does not exist; the cache is built inline via struct literal at line 124 (approx). No constructor to move.

- [ ] **Step 5: Run `goimports` on both files**

Run: `goimports -w cmd/helpers.go cmd/enis.go`

Expected: no unused-import warnings. If `"sync"` drops from `cmd/enis.go` because the cache type moved away, that's correct.

- [ ] **Step 6: Build / vet / test / race**

Run: `go build ./... && go vet ./... && go test ./... && go test -race ./cmd/...`
Expected: all clean. `cmd/pipeline_test.go`'s 8 `TestRunOrphanPipeline_*` still pass.

- [ ] **Step 7: Commit**

```bash
git add cmd/helpers.go cmd/enis.go
git commit -m "$(cat <<'EOF'
refactor(cmd): relocate CSV helpers and rename eniNameCache to awsNameCache

Moves splitCSV, mergeCSV, and getFlagString from cmd/enis.go to
cmd/helpers.go so future commands that need CSV-style filters can reuse
them without re-implementing or grep-discovering them in the wrong file.

Renames eniNameCache to awsNameCache and moves its type definition to
cmd/helpers.go. The struct's shape (VPC / subnet / SG string maps with a
sync.RWMutex) is generic enough to be reused by any command that
enriches AWS resource IDs with Name-tag lookups. The three methods that
use the cache (getVPCName, getSubnetName, ensureSGNames) stay on
*AWSCommand in cmd/enis.go because they are ENI-specific enrichment
wrappers.

Pure code movement with a type rename; no behavior change.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2 — Phase 2: `OrphanPipeline.ResourceLabel` + 11 spec populations

**Files:**
- Modify: `cmd/pipeline.go` (add field; add log emission)
- Modify: `cmd/pipeline_test.go` (extend one test to assert the summary log)
- Modify: 11 files — `cmd/volumes.go`, `cmd/enis.go`, `cmd/elasticaddresses.go`, `cmd/lambda.go`, `cmd/elasticache.go`, `cmd/opensearch.go`, `cmd/s3buckets.go`, `cmd/dynamodb.go`, `cmd/targetgroups.go`, `cmd/natgateways.go`, `cmd/ecs.go` — add one field to each spec literal.

### Steps

- [ ] **Step 1: Add `ResourceLabel` field to `OrphanPipeline` struct**

Edit `cmd/pipeline.go`. In the `OrphanPipeline[Item, Result any]` struct declaration, add `ResourceLabel string` between `Headers []string` and `HideIndex bool`. Final shape:

```go
type OrphanPipeline[Item, Result any] struct {
    Headers []string

    // ResourceLabel is the plural human-readable name of the resource type
    // (e.g. "Lambda functions", "ENIs"). When non-empty, runOrphanPipeline
    // emits "Found %d orphaned <ResourceLabel>" via Logger.LogInfo after
    // streaming all rows and before the delete phase.
    ResourceLabel string

    // HideIndex suppresses the leading "#" index column. Default false
    // ... (existing doc comment unchanged)
    HideIndex bool

    // List, Process, ToRow, Finalize, Delete — all unchanged
    // ... (rest of struct unchanged)
}
```

- [ ] **Step 2: Emit the summary log in `runOrphanPipeline`**

In `cmd/pipeline.go`'s `runOrphanPipeline` function, find the block after `<-collectorDone` and before the `if !collectDeletes || len(collected) == 0 { return nil }` check. Insert:

```go
    if spec.ResourceLabel != "" {
        a.Logger.LogInfo(fmt.Sprintf("Found %d orphaned %s", len(collected), spec.ResourceLabel), nil)
    }
```

Add `"fmt"` to the imports block of `cmd/pipeline.go`. Place it alphabetically between `"context"` and `"io"`.

- [ ] **Step 3: Update `TestRunOrphanPipeline_HappyPath_StreamsAllRows` to verify the log**

Edit `cmd/pipeline_test.go`. In `TestRunOrphanPipeline_HappyPath_StreamsAllRows`, modify the `OrphanPipeline` literal to include `ResourceLabel: "test items"`:

```go
    err := awsCmd.runOrphanPipeline(context.Background(), baseFlagValues(false), OrphanPipeline[int, string]{
        Headers:       []string{"Index", "Value"},
        ResourceLabel: "test items",  // NEW
        List: func(ctx context.Context, emit func(int) error) error {
            // ... unchanged
        },
        // ... rest of spec unchanged
    })
```

**IMPORTANT:** the existing test helper `newTestAWSCommand(out, promp)` creates an `AWSCommand` whose `Logger` uses `logging.NewLogger()`. That logger writes to `os.Stdout` (not the `out` buffer). The "Found N" log line will appear on stdout, not in the buffer the test reads. To assert the log, the test needs to either:

(a) Capture stdout (`os.Stdout` redirection) for the duration of the call, OR
(b) Introduce a `*logging.Logger` that writes to the same `out` buffer as the stream.

Option (b) is cleaner and doesn't fight the test framework. Update `newTestAWSCommand` in `cmd/pipeline_test.go`:

```go
func newTestAWSCommand(out *bytes.Buffer, promp prompter.Client) *AWSCommand {
    return &AWSCommand{
        Logger:   logging.NewLoggerWithOutput(out),  // NEW: use the buffer
        Prompter: promp,
        Output:   out,
    }
}
```

But `logging.NewLoggerWithOutput` doesn't exist. Adding it would widen Phase 2's scope.

**Alternative resolution:** skip the log-emission assertion in the test and rely on the other seven tests + smoke-testing. Add a minimal sanity check via a different route: assert `spec.ResourceLabel` is preserved through one more indirect path. Or, simpler: add a package-private `var loggerForTesting *logging.Logger` that tests can swap.

For this task, take the **simplest path**: keep the test change minimal. Assert that the `ResourceLabel` field itself can be set without errors by running the test unchanged structurally — the new field is optional. Do NOT modify test-assertion logic in Phase 2.

Simplified Step 3: set `ResourceLabel: "test items"` in the existing happy-path test but do NOT add a new assertion about the log output. The log will emit to stdout during the test run but won't be asserted on. If the test framework captures output via the buffer, the log line appears there; if not, it goes to stdout. Either way, the test still passes.

**If `go test` shows the log emission is causing failures** (e.g. Logger writes a trailing newline to stdout that breaks some other test), retreat to Option (b) — add `logging.NewLoggerWithOutput(w io.Writer) *Logger` to `pkg/handlers/logging/logging.go` and use it. That's a two-line additive change to the logging package, acceptable scope creep.

Start with the simplified version and only expand if tests fail.

- [ ] **Step 4: Populate `ResourceLabel` in the 11 command files**

For each of the 11 files listed below, find the `OrphanPipeline[Item, Result]{...}` literal inside `executeX` and add `ResourceLabel: "<label>"` right after `Headers: ...`. The exact literal string per file:

| File | Literal to add |
|---|---|
| `cmd/volumes.go` | `ResourceLabel: "EBS volumes",` |
| `cmd/enis.go` | `ResourceLabel: "ENIs",` |
| `cmd/elasticaddresses.go` | `ResourceLabel: "Elastic IPs",` |
| `cmd/lambda.go` | `ResourceLabel: "Lambda functions",` |
| `cmd/elasticache.go` | `ResourceLabel: "ElastiCache clusters",` |
| `cmd/opensearch.go` | `ResourceLabel: "OpenSearch domains",` |
| `cmd/s3buckets.go` | `ResourceLabel: "S3 buckets",` |
| `cmd/dynamodb.go` | `ResourceLabel: "DynamoDB tables",` |
| `cmd/targetgroups.go` | `ResourceLabel: "target groups",` |
| `cmd/natgateways.go` | `ResourceLabel: "NAT Gateways",` |
| `cmd/ecs.go` | `ResourceLabel: "ECS clusters",` |

Place each on the line immediately after `Headers: [...],` and before either `HideIndex:` (if present) or `List:`.

- [ ] **Step 5: Build / vet / test / race**

Run: `go build ./... && go vet ./... && go test ./... && go test -race ./cmd/...`
Expected: all clean. 8 `TestRunOrphanPipeline_*` tests pass.

- [ ] **Step 6: Smoke-test one command**

Run: `go run . volumes --help`
Expected: usage block prints without error. (The summary log only emits when the command actually runs against AWS, which we're not doing in smoke tests.)

- [ ] **Step 7: Commit**

```bash
git add cmd/pipeline.go cmd/pipeline_test.go cmd/volumes.go cmd/enis.go cmd/elasticaddresses.go cmd/lambda.go cmd/elasticache.go cmd/opensearch.go cmd/s3buckets.go cmd/dynamodb.go cmd/targetgroups.go cmd/natgateways.go cmd/ecs.go
git commit -m "$(cat <<'EOF'
refactor(cmd/pipeline): add ResourceLabel and restore "Found N" summary log

Refactor B retired each pipeline command's pre-refactor
Logger.LogInfo("Found %d orphaned X", ...) summary line because the
shared helper had no way to name the resource type. Add
OrphanPipeline.ResourceLabel (optional) so the helper can emit the
summary line when the field is non-empty, and populate it for the 11
converted commands with labels matching the pre-refactor wording.

Emits only when ResourceLabel != "", so future pipeline commands that
don't want the summary (if any) can opt out by omitting the field. The
log fires after the collector drains (i.e., all rows are streamed) and
before the delete-confirm prompt, matching the pre-refactor sequence.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3 — Phase 3: `GlobalFlags` typed struct flip (atomic)

**Why atomic:** `flags.GetFlags` is called by `runResourceCommand`, which is called by every command's `RunE`. Changing `GetFlags`'s return shape breaks every `executeX` that receives the result. Go's build system refuses to compile a package with any unresolved type error. To avoid landing a broken tree in any commit, this task does the full signature flip in one atomic commit touching ~28 files.

The subagent works through this task **sequentially** — one file at a time — but **commits only once at the end** after all files compile. If any intermediate step fails, they fix and continue; the commit is only made when `go build ./...` is green.

**Files:**
- Modify: `pkg/handlers/flags/flags.go` (add type; rewrite `GetFlags`)
- Modify: `pkg/handlers/flags/flags_test.go` (update 3 existing tests; add 1 new)
- Modify: `cmd/runner.go` (signature + internal reads)
- Modify: `cmd/runner_test.go` (update `TestRunResourceCommand_HappyPath`)
- Modify: `cmd/pipeline.go` (signature + 2 internal reads)
- Modify: `cmd/pipeline_test.go` (update all 8 `TestRunOrphanPipeline_*`)
- Modify: 25 `cmd/<resource>.go` files (each `executeX` signature + global-flag reads; per-command reads rename `flagValues` → `extras`)

### Conversion recipe (applied uniformly to all 25 `executeX` methods)

Before:
```go
func (v *AWSCommand) executeVolumes(ctx context.Context, flagValues *map[string]any) error {
    // ... globals read via (*flagValues)["delete"].(bool), etc.
    // ... per-command flags read via (*flagValues)["filter-by-name"].(string)
    return runOrphanPipeline(v, ctx, flagValues, OrphanPipeline[...]{...})  // pipeline commands only
}
```

After:
```go
func (v *AWSCommand) executeVolumes(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
    // ... globals read via globals.Delete, globals.SortBy, globals.SortDesc
    // ... per-command flags read via (*extras)["filter-by-name"].(string)
    return runOrphanPipeline(v, ctx, globals, extras, OrphanPipeline[...]{...})  // pipeline commands only
}
```

Substitutions inside each body:
- `(*flagValues)["delete"].(bool)` → `globals.Delete`
- `(*flagValues)["sort-by"].(string)` → `globals.SortBy`
- `(*flagValues)["sort-desc"].(bool)` → `globals.SortDesc`
- Any `(*flagValues)["<per-command-flag>"].(T)` → `(*extras)["<per-command-flag>"].(T)` (just renamed).
- Any `flagValues *map[string]any` parameter → `extras *map[string]any` (just renamed).

Inside `RunE` callers, the method-expression binding stays `(*AWSCommand).executeVolumes` — the signature change is uniform so the method-expression type still matches what `runResourceCommand` expects.

### Steps

- [ ] **Step 1: Add `GlobalFlags` type to `pkg/handlers/flags/flags.go`**

Insert near the top of `flags.go` (after the imports, before any existing type/func):

```go
// GlobalFlags holds the six root-level flags that every command shares.
// Commands read them directly from this struct; the extras map returned
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

- [ ] **Step 2: Rewrite `GetFlags` to return `(*GlobalFlags, *map[string]any, error)`**

Replace the entire body of `GetFlags` with:

```go
// GetFlags resolves the six baseline global flags into a typed *GlobalFlags
// and the caller-supplied additional flags into a *map[string]any (extras).
// Precedence for every value is CLI > viper (config file + env) > defaults —
// unchanged from the pre-typed version.
func GetFlags(flagRetriever FlagRetriever, additionalFlags []Flag) (*GlobalFlags, *map[string]any, error) {
    getString := func(name string) (string, error) {
        if !flagRetriever.IsChanged(name) && viper.IsSet(name) {
            return viper.GetString(name), nil
        }
        return flagRetriever.GetString(name)
    }
    getBool := func(name string) (bool, error) {
        if !flagRetriever.IsChanged(name) && viper.IsSet(name) {
            return viper.GetBool(name), nil
        }
        return flagRetriever.GetBool(name)
    }

    globals := &GlobalFlags{}
    var err error
    if globals.Region, err = getString("region"); err != nil {
        return nil, nil, err
    }
    if globals.Profile, err = getString("profile"); err != nil {
        return nil, nil, err
    }
    if globals.AuthMethod, err = getString("auth-method"); err != nil {
        return nil, nil, err
    }
    if globals.Delete, err = getBool("delete"); err != nil {
        return nil, nil, err
    }
    if globals.SortBy, err = getString("sort-by"); err != nil {
        return nil, nil, err
    }
    if globals.SortDesc, err = getBool("sort-desc"); err != nil {
        return nil, nil, err
    }

    extras := make(map[string]any, len(additionalFlags))
    for _, f := range additionalFlags {
        switch f.Type {
        case "string":
            v, err := getString(f.Name)
            if err != nil {
                return nil, nil, err
            }
            extras[f.Name] = v
        case "bool":
            v, err := getBool(f.Name)
            if err != nil {
                return nil, nil, err
            }
            extras[f.Name] = v
        case "int":
            if !flagRetriever.IsChanged(f.Name) && viper.IsSet(f.Name) {
                extras[f.Name] = viper.GetInt(f.Name)
                continue
            }
            v, err := flagRetriever.GetInt(f.Name)
            if err != nil {
                return nil, nil, err
            }
            extras[f.Name] = v
        default:
            return nil, nil, fmt.Errorf("unsupported flag type: %s", f.Type)
        }
    }
    return globals, &extras, nil
}
```

Delete the old `baseFlags` slice declaration and the combined-for-loop body that populated a single `map[string]any`. Keep the `FlagRetriever` interface and `CommandFlagRetriever` struct + methods (`GetString`, `GetBool`, `GetInt`, `IsChanged`) unchanged.

- [ ] **Step 3: Update existing tests in `pkg/handlers/flags/flags_test.go`**

Three existing tests currently assert on `(*got)["region"].(string)` etc. Update them to use the new two-value return:

```go
func TestGetFlags_ViperFallbackWhenNotChanged(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    viper.Set("region", "us-west-2")
    viper.Set("delete", true)

    r := &fakeRetriever{
        strings: map[string]string{"region": "us-east-1"},
        bools:   map[string]bool{"delete": false},
        changed: map[string]bool{},
    }

    globals, _, err := GetFlags(r, nil)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if globals.Region != "us-west-2" {
        t.Fatalf("expected region from viper, got %q", globals.Region)
    }
    if globals.Delete != true {
        t.Fatalf("expected delete from viper, got %v", globals.Delete)
    }
}

func TestGetFlags_CLIWinsOverViper(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    viper.Set("profile", "from-config")

    r := &fakeRetriever{
        strings: map[string]string{"profile": "from-cli"},
        bools:   map[string]bool{},
        changed: map[string]bool{"profile": true},
    }

    globals, _, err := GetFlags(r, nil)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if globals.Profile != "from-cli" {
        t.Fatalf("expected profile from CLI, got %q", globals.Profile)
    }
}

func TestGetFlags_AdditionalFlagsAndUnsupportedType(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    r := &fakeRetriever{
        strings: map[string]string{"filter-by-name": "abc"},
        bools:   map[string]bool{},
        changed: map[string]bool{"filter-by-name": true},
    }

    _, extras, err := GetFlags(r, []Flag{{Name: "filter-by-name", Type: "string"}})
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if (*extras)["filter-by-name"].(string) != "abc" {
        t.Fatalf("expected additional flag, got %q", (*extras)["filter-by-name"])
    }

    _, _, err = GetFlags(r, []Flag{{Name: "x", Type: "float"}})
    if err == nil {
        t.Fatalf("expected error for unsupported type")
    }
}
```

- [ ] **Step 4: Add new test `TestGetFlags_ReturnsTypedGlobals` to `flags_test.go`**

Append to the end of the test file:

```go
func TestGetFlags_ReturnsTypedGlobals(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    r := &fakeRetriever{
        strings: map[string]string{
            "region":      "us-west-2",
            "profile":     "prod",
            "auth-method": "ENV_SECRET",
            "sort-by":     "Name",
            "filter-by-name": "x",
        },
        bools: map[string]bool{
            "delete":    true,
            "sort-desc": true,
        },
        changed: map[string]bool{
            "region": true, "profile": true, "auth-method": true,
            "delete": true, "sort-by": true, "sort-desc": true,
            "filter-by-name": true,
        },
    }

    globals, extras, err := GetFlags(r, []Flag{{Name: "filter-by-name", Type: "string"}})
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if globals.Region != "us-west-2" {
        t.Errorf("Region: expected us-west-2, got %q", globals.Region)
    }
    if globals.Profile != "prod" {
        t.Errorf("Profile: expected prod, got %q", globals.Profile)
    }
    if globals.AuthMethod != "ENV_SECRET" {
        t.Errorf("AuthMethod: expected ENV_SECRET, got %q", globals.AuthMethod)
    }
    if !globals.Delete {
        t.Error("Delete: expected true")
    }
    if globals.SortBy != "Name" {
        t.Errorf("SortBy: expected Name, got %q", globals.SortBy)
    }
    if !globals.SortDesc {
        t.Error("SortDesc: expected true")
    }
    // Extras must contain only the additional flag, not the globals.
    if _, ok := (*extras)["region"]; ok {
        t.Error("extras should not contain global 'region'")
    }
    if _, ok := (*extras)["filter-by-name"]; !ok {
        t.Error("extras should contain additional 'filter-by-name'")
    }
    if (*extras)["filter-by-name"].(string) != "x" {
        t.Errorf("extras[filter-by-name]: expected x, got %q", (*extras)["filter-by-name"])
    }
}
```

- [ ] **Step 5: Update `cmd/runner.go` — `runResourceCommand` signature and body**

Replace the `runResourceCommand` function signature and body with:

```go
func runResourceCommand(
    cmd *cobra.Command,
    setup CommandSetup,
    execute func(awsCmd *AWSCommand, ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error,
) error {
    ctx := cmd.Context()

    flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
    globals, extras, err := flags.GetFlags(flagRetriever, setup.AdditionalFlags)
    if err != nil {
        return err
    }

    cloudConfig := &handlers.CloudConfig{
        AuthMethod: aws.String(globals.AuthMethod),
        Profile:    aws.String(globals.Profile),
        Region:     aws.String(globals.Region),
    }
    cfg, err := newConfigFn(ctx, *cloudConfig, "UTC", true, true)
    if err != nil {
        return err
    }

    client := setup.BuildClients(cfg)
    if client == nil {
        return fmt.Errorf("runResourceCommand: BuildClients returned nil")
    }
    awsCmd := newAWSCommand(client, cloudConfig, os.Stdin, os.Stdout)
    return execute(awsCmd, ctx, globals, extras)
}
```

The function-signature doc comment above `runResourceCommand` stays as-is (the "argument order is load-bearing" note) — but update it to mention `globals, extras` instead of `flagValues`.

- [ ] **Step 6: Update `cmd/runner_test.go` — `TestRunResourceCommand_HappyPath`**

Replace the captured-value assertions to use the new callback signature. In `TestRunResourceCommand_HappyPath`, change:

```go
    execute := func(awsCmd *AWSCommand, ctx context.Context, fv *map[string]any) error {
        executeCalled = true
        capturedAWSCmd = awsCmd
        capturedFlagValues = fv
        return nil
    }
```

to:

```go
    var (
        capturedGlobals *flags.GlobalFlags
        capturedExtras  *map[string]any
    )
    execute := func(awsCmd *AWSCommand, ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
        executeCalled = true
        capturedAWSCmd = awsCmd
        capturedGlobals = globals
        capturedExtras = extras
        return nil
    }
```

Drop the old `capturedFlagValues` variable. Update the subsequent assertions:

- Replace `(*capturedFlagValues)["region"].(string)` with `capturedGlobals.Region`.
- Replace the `expectedKeys := []string{"region", "profile", ...}` loop with two blocks: one asserting every field of `*capturedGlobals` has its expected value, and one asserting `(*capturedExtras)["filter-by-name"]` exists.

- [ ] **Step 7: Update `cmd/pipeline.go` — `runOrphanPipeline` signature and internal reads**

Change the `runOrphanPipeline` signature from:

```go
func runOrphanPipeline[Item, Result any](
    a *AWSCommand,
    ctx context.Context,
    flagValues *map[string]any,
    spec OrphanPipeline[Item, Result],
) error
```

to:

```go
func runOrphanPipeline[Item, Result any](
    a *AWSCommand,
    ctx context.Context,
    globals *flags.GlobalFlags,
    extras *map[string]any,
    spec OrphanPipeline[Item, Result],
) error
```

Inside the body, change:
- `collectDeletes := (*flagValues)["delete"].(bool) && spec.Delete != nil` → `collectDeletes := globals.Delete && spec.Delete != nil`
- `stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))` → `stream.SetSort(globals.SortBy, globals.SortDesc)`
- Remove the now-unused `extras` parameter references? No — `extras` is passed through but not used inside the helper today. It may be used by future enhancements; keep it as an unused parameter for now. Actually, if unused, Go's compiler will complain. **Rename the parameter to `_` in the signature** OR use `_ = extras` at the top of the function body to silence the linter. Cleanest: name it `_` in the parameter list since the helper genuinely doesn't read from extras.

Wait — the helper SHOULD take `extras` so callers have a place to pass it through consistently. But if unused, `_ = extras` is fine. Go does NOT actually error on unused parameters (only unused variables). So leave `extras *map[string]any` named and ignore the fact that the body doesn't use it. No `_` alias needed.

- [ ] **Step 8: Update `cmd/pipeline_test.go` — all 8 `TestRunOrphanPipeline_*` tests**

The helper `baseFlagValues(delete bool) *map[string]any` no longer fits the new signature. Rewrite it:

```go
// baseGlobals returns a *flags.GlobalFlags with sensible test defaults.
func baseGlobals(delete bool) *flags.GlobalFlags {
    return &flags.GlobalFlags{
        Region:     "us-east-1",
        Profile:    "default",
        AuthMethod: "AWS_CREDENTIALS_FILE",
        Delete:     delete,
        SortBy:     "",
        SortDesc:   false,
    }
}

// baseExtras returns an empty extras map.
func baseExtras() *map[string]any {
    m := map[string]any{}
    return &m
}
```

Delete the old `baseFlagValues`. Then update each of the 8 test functions' `runOrphanPipeline` calls. The old call:

```go
err := awsCmd.runOrphanPipeline(context.Background(), baseFlagValues(false), OrphanPipeline[...]{...})
```

Wait — it was `awsCmd.runOrphanPipeline(...)`, but after Task 1 of the pipeline refactor we switched to package-level function form: `runOrphanPipeline(awsCmd, ctx, ...)`. Double-check current form in `cmd/pipeline_test.go` before editing. If the current form is `runOrphanPipeline(awsCmd, ctx, baseFlagValues(false), spec)`, the update is:

```go
err := runOrphanPipeline(awsCmd, context.Background(), baseGlobals(false), baseExtras(), OrphanPipeline[...]{...})
```

Apply this transformation to all 8 test functions. Each replaces `baseFlagValues(x)` with `baseGlobals(x), baseExtras()` — two separate arguments instead of one.

- [ ] **Step 9: Update all 25 `executeX` methods**

For each file below, apply the conversion recipe from the task header:

1. Change the method signature from `(ctx context.Context, flagValues *map[string]any) error` to `(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error`.
2. Find every `(*flagValues)["delete"].(bool)` in the method body and replace with `globals.Delete`.
3. Find every `(*flagValues)["sort-by"].(string)` and replace with `globals.SortBy`.
4. Find every `(*flagValues)["sort-desc"].(bool)` and replace with `globals.SortDesc`.
5. Rename the remaining `flagValues` parameter references to `extras`. For per-command flags like `(*flagValues)["filter-by-name"].(string)` this becomes `(*extras)["filter-by-name"].(string)` — just the variable name changes.
6. For pipeline-using commands, update the `runOrphanPipeline` call to pass both: `runOrphanPipeline(<receiver>, ctx, globals, extras, OrphanPipeline[...]{...})`.
7. If the method imports only `flagValues` via its parameter name (no references to `flag` or `flags`), add `"github.com/pincher95/cor/pkg/handlers/flags"` to the imports block so the new `*flags.GlobalFlags` type is in scope.

The 25 files:

```
cmd/autoscaling.go          cmd/logs.go              cmd/snapshots.go
cmd/clientvpn.go            cmd/natgateways.go       cmd/targetgroups.go
cmd/dynamodb.go             cmd/opensearch.go        cmd/tgwattachments.go
cmd/ecr.go                  cmd/rds.go               cmd/volumes.go
cmd/ecs.go                  cmd/route53zones.go      cmd/vpcendpoints.go
cmd/efs.go                  cmd/s3buckets.go         cmd/vpnconnections.go
cmd/elasticache.go          cmd/elbv1.go             cmd/elasticaddresses.go
cmd/elbv2.go                cmd/enis.go              cmd/images.go
cmd/lambda.go
```

Work through them in any order. After each file is edited, `goimports -w cmd/<file>.go` to clean imports.

**Sanity check during the loop:** after each file, you don't have to run `go build` — it'll fail until all 25 are done. Only run the build at Step 10 below.

- [ ] **Step 10: Run `goimports` across the batch**

Run: `goimports -w cmd/*.go pkg/handlers/flags/flags.go pkg/handlers/flags/flags_test.go`

Expected: no drift beyond what the conversions need.

- [ ] **Step 11: Build and diagnose**

Run: `go build ./...`

Expected: success. If it fails, read the error output, fix the referenced file, and retry. Common failures:
- Forgot to add `"github.com/pincher95/cor/pkg/handlers/flags"` to an imports block.
- Forgot to rename `flagValues` to `extras` in one spot (Go reports the symbol as undefined).
- Typo on `globals.SortBy` vs `globals.SortBy` (none, it's one symbol, but watch for casing).

Iterate until `go build ./...` returns clean.

- [ ] **Step 12: Run vet, test, and race**

Run: `go vet ./... && go test ./... && go test -race ./cmd/...`
Expected: clean. Test output must show:
- 4 tests in `pkg/handlers/flags/flags_test.go` (3 updated + 1 new)
- `TestRunResourceCommand_HappyPath` in `cmd/runner_test.go`
- 8 `TestRunOrphanPipeline_*` in `cmd/pipeline_test.go`

All 13 tests above + any other existing tests must pass.

- [ ] **Step 13: Smoke tests**

Run these three commands, each should print a help block without error:

```
go run . volumes --help
go run . autoscaling --help
go run . lambda --help
```

Expected: help pages render normally. No panic on flag parsing, no type assertion errors.

- [ ] **Step 14: Commit atomically**

```bash
git add pkg/handlers/flags/flags.go pkg/handlers/flags/flags_test.go cmd/
git commit -m "$(cat <<'EOF'
refactor(cmd): introduce GlobalFlags typed struct and flip all executeX signatures

Replaces the pre-typed (*map[string]any) flag-retrieval path with a typed
*flags.GlobalFlags for the six root-level flags (region, profile,
auth-method, delete, sort-by, sort-desc). Per-command flags continue to
flow through an untyped extras map — a future cleanup may type those
per-command, but the biggest type-assertion-panic surface (the 6 globals
read from ~30 call sites) is now eliminated.

GetFlags now returns (*GlobalFlags, *map[string]any, error). The runner
and pipeline helpers take the typed globals and read fields directly;
every executeX method's signature adds a *flags.GlobalFlags parameter
between ctx and the renamed extras map. Per-command flag reads change
only the container name (flagValues -> extras); per-command keys and
types are unchanged.

This is one atomic commit touching 28 files because Go's build system
refuses to compile partial-signature changes across a single package.
Tests updated in both pkg/handlers/flags and cmd/. No behavior change.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

If pre-commit hooks fail at this commit, fix the underlying issue and create a NEW follow-up commit. Do not use `--amend` or `--no-verify`.

---

## Task 4 — Final verification and cleanup

**Files:** read-only audits across `cmd/` and `pkg/`.

### Steps

- [ ] **Step 1: Zero `(*flagValues)` references in cmd/**

Run: `grep -rn "(\*flagValues)" cmd/`
Expected: no output (no remaining map-cast accesses anywhere).

- [ ] **Step 2: All 11 pipeline commands have `ResourceLabel`**

Run: `grep -rn "ResourceLabel:" cmd/`
Expected: 11 hits (one per converted pipeline command), plus optionally one reference in `cmd/pipeline_test.go` if the test literal sets it.

- [ ] **Step 3: `awsNameCache` defined in helpers.go, referenced only in enis.go + helpers.go**

Run: `grep -rn "awsNameCache\|eniNameCache" cmd/`
Expected:
- `cmd/helpers.go` contains the type declaration.
- `cmd/enis.go` contains 5 references (cache literal + 4 method param types + `formatSGList` param type).
- Zero hits for the old `eniNameCache` name.

- [ ] **Step 4: GlobalFlags referenced in the right places**

Run: `grep -l "flags.GlobalFlags" cmd/ pkg/`
Expected files: `pkg/handlers/flags/flags.go`, `pkg/handlers/flags/flags_test.go`, `cmd/runner.go`, `cmd/runner_test.go`, `cmd/pipeline.go`, `cmd/pipeline_test.go`, plus all 25 `executeX` command files.

Quick sanity check: `grep -l "flags.GlobalFlags" cmd/*.go | wc -l` should return `29` (25 commands + `runner.go` + `runner_test.go` + `pipeline.go` + `pipeline_test.go`).

- [ ] **Step 5: Full test suite green**

Run: `go build ./... && go vet ./... && go test ./... && go test -race ./cmd/...`
Expected: clean.

- [ ] **Step 6: Full pre-commit**

Run: `pre-commit run --all-files`
Expected: all hooks pass.

If any hook surfaces a fix, apply it and commit as a follow-up `style(cmd):` commit (do NOT `--amend` Task 3's big commit).

- [ ] **Step 7: LOC delta check**

Run: `wc -l cmd/*.go | tail -1`

Record the number. Compare against the baseline recorded in Task 6 of refactor B (6,460 LOC). Expected delta from this sweep: near zero (small positive for Phase 3 — adds `GlobalFlags` param to 25 function signatures, ~25 LOC; offset by small simplifications when `(*flagValues)["x"].(T)` becomes `globals.X`, ~25 LOC saved). Net: within ±50 LOC.

If delta is drastically outside ±100 LOC, investigate — something was added or deleted that wasn't in the plan.

- [ ] **Step 8: No follow-up commit needed (skip if Step 6 was green)**

If Step 6 was clean, no commit is required for Task 4. Done.

If Step 6 surfaced a fix, stage and commit:

```bash
git add -u
git commit -m "$(cat <<'EOF'
style(cmd): post-sweep lint cleanup

Post-refactor lint pass after the cleanup sweep landed.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Definition of done

After Task 4 completes:

- `cmd/helpers.go` contains `splitCSV`, `mergeCSV`, `getFlagString`, and `awsNameCache` (renamed from `eniNameCache`).
- `OrphanPipeline.ResourceLabel` exists. 11 pipeline commands populate it with pre-refactor-matched labels.
- `flags.GlobalFlags` type exists. `flags.GetFlags` returns `(*GlobalFlags, *map[string]any, error)`.
- All 25 `executeX` methods take `(*AWSCommand, context.Context, *flags.GlobalFlags, *map[string]any) error`.
- Zero `(*flagValues)` map-cast references in `cmd/` or `pkg/`.
- `go build/vet/test/race/pre-commit` clean.
- No user-visible behavior change beyond the restored `"Found N orphaned X"` summary log.
