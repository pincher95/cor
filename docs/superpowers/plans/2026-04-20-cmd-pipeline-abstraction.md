# `cmd/` Pipeline Abstraction Refactor — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse the ~500 LOC of producer → workers → collector errgroup ceremony that 11 of the 15 pipeline-shaped commands in `cmd/` share into a generic helper `runOrphanPipeline[I, R]`, and convert those 11 commands to use it.

**Architecture:** A new file `cmd/pipeline.go` exports `runOrphanPipeline` (method on `*AWSCommand`) + a generic spec struct `OrphanPipeline[I, R]` with fields for Headers/List/Process/ToRow and optional Finalize/Delete hooks. The helper owns the errgroup, worker pool, collector goroutine, stream table, and the `confirm-and-delete` flow including the `rootCtx := ctx` preservation. Each converted command's `executeX` becomes one `return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[...]{ ... })` call whose spec literal carries the command-specific List/Process/ToRow/Delete closures. Existing enrichment helpers (e.g. `checkLambdaOrphan`, `abortMultipartUploads`) are kept; `Process` / `Delete` closures call them.

**Tech Stack:** Go 1.26.2, Cobra + Viper, AWS SDK Go v2, `golang.org/x/sync/errgroup`, stdlib `testing`.

**Spec:** `docs/superpowers/specs/2026-04-20-cmd-pipeline-abstraction-design.md`.

---

## Prerequisites

Read `CLAUDE.md` for:
- Commit convention (`commitlint` with required scope + blank line + body; header ≤120 chars, body lines ≤100 chars).
- Pre-commit hooks (`go-fmt`, `go-vet`, `go-imports`, `golangci-lint`, `go-unit-tests`, `go-build`, `go-mod-tidy`).
- The `rootCtx := ctx` pattern (now lives inside `runOrphanPipeline`, not per-command).

Read `docs/superpowers/specs/2026-04-20-cmd-pipeline-abstraction-design.md` for the design rationale and `OrphanPipeline` contract.

Read `cmd/runner.go` — the refactor "A" helper that every command already goes through. `runOrphanPipeline` is a sibling helper called from inside `executeX`, not a replacement.

### Out of scope (stays custom)

Do **not** touch these four pipeline commands in this refactor — their shapes don't fit the standard helper:

- `cmd/rds.go` — two conditional producers feeding one stream
- `cmd/logs.go` — inline pagination, no separate producer goroutine
- `cmd/images.go` — nested pagination inside workers
- `cmd/elbv2.go` — three `g.Go` blocks (extra listener-deletion pipeline)

Also out of scope: `flagValues` typed struct, receiver-name normalization, `AWSClientImpl` cleanup.

### Target files (11 commands)

volumes, enis, elasticaddresses, lambda, elasticache, opensearch, s3buckets, dynamodb, targetgroups, natgateways, ecs.

Pre-refactor baseline: `cmd/` is 6,806 LOC total. Expected reduction: ~400-500 LOC after the helper's ~120 new lines (`pipeline.go` + `pipeline_test.go`) are netted out.

---

## Task 1: Add `cmd/pipeline.go` and its test (TDD)

**Files:**
- Create: `cmd/pipeline_test.go`
- Create: `cmd/pipeline.go`

### Steps

- [ ] **Step 1: Write the failing test**

Create `cmd/pipeline_test.go` with exactly this content:

```go
/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
    "bytes"
    "context"
    "testing"

    "github.com/pincher95/cor/pkg/handlers/logging"
    "github.com/pincher95/cor/pkg/handlers/prompter"
    "github.com/spf13/viper"
)

type fakePrompter struct {
    answer bool
    err    error
}

func (f *fakePrompter) Confirm(prompt string) (*bool, error) {
    if f.err != nil {
        return nil, f.err
    }
    return &f.answer, nil
}

func newTestAWSCommand(out *bytes.Buffer, promp prompter.Client) *AWSCommand {
    return &AWSCommand{
        Logger:   logging.NewLogger(),
        Prompter: promp,
        Output:   out,
    }
}

// baseFlagValues returns a flagValues map with the six global flags populated
// with sensible test defaults. Tests mutate individual keys as needed.
func baseFlagValues(delete bool) *map[string]any {
    m := map[string]any{
        "region":      "us-east-1",
        "profile":     "default",
        "auth-method": "AWS_CREDENTIALS_FILE",
        "delete":      delete,
        "sort-by":     "",
        "sort-desc":   false,
    }
    return &m
}

func TestRunOrphanPipeline_HappyPath_StreamsAllRows(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    out := &bytes.Buffer{}
    promp := &fakePrompter{answer: false}
    awsCmd := newTestAWSCommand(out, promp)

    var processed int
    var finalizeCalled bool

    err := awsCmd.runOrphanPipeline(context.Background(), baseFlagValues(false), OrphanPipeline[int, string]{
        Headers: []string{"Index", "Value"},
        List: func(ctx context.Context, emit func(int) error) error {
            for i := 1; i <= 3; i++ {
                if err := emit(i); err != nil {
                    return err
                }
            }
            return nil
        },
        Process: func(_ context.Context, item int) (*string, error) {
            processed++
            s := ""
            for j := 0; j < item; j++ {
                s += "x"
            }
            return &s, nil
        },
        ToRow: func(r string) []any {
            return []any{len(r), r}
        },
        Finalize: func(results []string) []any {
            finalizeCalled = true
            return []any{"Total", len(results)}
        },
    })
    if err != nil {
        t.Fatalf("runOrphanPipeline returned error: %v", err)
    }
    if processed != 3 {
        t.Errorf("expected Process to be called 3 times, got %d", processed)
    }
    if !finalizeCalled {
        t.Error("expected Finalize to be called")
    }
    output := out.String()
    // Each of "x", "xx", "xxx" plus Finalize "Total 3" should appear in output.
    for _, want := range []string{"x", "xx", "xxx", "Total"} {
        if !bytes.Contains([]byte(output), []byte(want)) {
            t.Errorf("expected output to contain %q; got:\n%s", want, output)
        }
    }
}

func TestRunOrphanPipeline_DeleteCalledWhenConfirmed(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    out := &bytes.Buffer{}
    promp := &fakePrompter{answer: true}
    awsCmd := newTestAWSCommand(out, promp)

    var deletes int

    err := awsCmd.runOrphanPipeline(context.Background(), baseFlagValues(true), OrphanPipeline[int, int]{
        Headers: []string{"Value"},
        List: func(ctx context.Context, emit func(int) error) error {
            for _, i := range []int{10, 20, 30} {
                if err := emit(i); err != nil {
                    return err
                }
            }
            return nil
        },
        Process: func(_ context.Context, item int) (*int, error) {
            v := item
            return &v, nil
        },
        ToRow:  func(r int) []any { return []any{r} },
        Delete: func(ctx context.Context, r int) error { deletes++; return nil },
    })
    if err != nil {
        t.Fatalf("runOrphanPipeline returned error: %v", err)
    }
    if deletes != 3 {
        t.Errorf("expected Delete to be called 3 times, got %d", deletes)
    }
}

func TestRunOrphanPipeline_DeleteNotCalledWhenFlagFalse(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    out := &bytes.Buffer{}
    promp := &fakePrompter{answer: true}
    awsCmd := newTestAWSCommand(out, promp)

    var deletes int

    err := awsCmd.runOrphanPipeline(context.Background(), baseFlagValues(false), OrphanPipeline[int, int]{
        Headers: []string{"Value"},
        List: func(ctx context.Context, emit func(int) error) error {
            return emit(1)
        },
        Process: func(_ context.Context, item int) (*int, error) { v := item; return &v, nil },
        ToRow:   func(r int) []any { return []any{r} },
        Delete:  func(ctx context.Context, r int) error { deletes++; return nil },
    })
    if err != nil {
        t.Fatalf("runOrphanPipeline returned error: %v", err)
    }
    if deletes != 0 {
        t.Errorf("expected Delete NOT to be called, got %d invocations", deletes)
    }
}

func TestRunOrphanPipeline_ProcessNilSkipsItem(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    out := &bytes.Buffer{}
    promp := &fakePrompter{answer: false}
    awsCmd := newTestAWSCommand(out, promp)

    var rows int

    err := awsCmd.runOrphanPipeline(context.Background(), baseFlagValues(false), OrphanPipeline[int, int]{
        Headers: []string{"Value"},
        List: func(ctx context.Context, emit func(int) error) error {
            for i := 1; i <= 4; i++ {
                if err := emit(i); err != nil {
                    return err
                }
            }
            return nil
        },
        Process: func(_ context.Context, item int) (*int, error) {
            if item%2 == 0 {
                return nil, nil // skip evens
            }
            v := item
            return &v, nil
        },
        ToRow: func(r int) []any { rows++; return []any{r} },
    })
    if err != nil {
        t.Fatalf("runOrphanPipeline returned error: %v", err)
    }
    if rows != 2 {
        t.Errorf("expected 2 rows (odd items only), got %d", rows)
    }
}
```

- [ ] **Step 2: Run the test to confirm compile failure**

Run: `go test ./cmd/ -run TestRunOrphanPipeline`
Expected: **compile error** mentioning `undefined: runOrphanPipeline`, `undefined: OrphanPipeline`.

- [ ] **Step 3: Implement `cmd/pipeline.go`**

Create `cmd/pipeline.go` with exactly this content:

```go
/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cmd

import (
    "context"

    "github.com/pincher95/cor/pkg/handlers/printer"
    "golang.org/x/sync/errgroup"
)

// OrphanPipeline declares per-command variation for runOrphanPipeline.
//
// I is the item type produced by List (typically an AWS SDK type such as
// ec2types.Volume). R is the result type produced by Process — a
// command-owned struct carrying the row cells plus whatever delete
// metadata Delete needs.
//
// Required fields: Headers, List, Process, ToRow.
// Optional: Finalize (write a footer row like totals), Delete (enables
// --delete for this command; nil makes --delete a no-op).
type OrphanPipeline[I, R any] struct {
    Headers []string

    // List paginates or lists the resource type. It must call emit once
    // per item; emit handles the context-aware handoff to the worker
    // pool. If emit returns a non-nil error, List must propagate it
    // (the error signals that the pipeline is shutting down).
    List func(ctx context.Context, emit func(I) error) error

    // Process performs per-item enrichment and filtering. Return (nil,
    // nil) to skip an item without signalling an error; return
    // (nil, err) to abort the whole pipeline.
    Process func(ctx context.Context, item I) (*R, error)

    // ToRow converts a result into the cells of a single streamed row.
    // The returned slice length must match len(Headers).
    ToRow func(r R) []any

    // Finalize runs after all rows are streamed; if it returns a
    // non-nil slice it is written as one final row (e.g. totals).
    // Optional.
    Finalize func(results []R) []any

    // Delete is invoked per result during the --delete phase with the
    // original, non-errgroup context so post-g.Wait() cancellations
    // don't block the delete call. Nil disables --delete for this
    // command.
    Delete func(ctx context.Context, r R) error
}

// runOrphanPipeline runs the shared producer → workers → collector pattern
// that the standard-shape commands in cmd/ share. See OrphanPipeline for
// the contract.
//
// The caller retains ownership of any AWS clients — the pipeline only
// touches the spec's callbacks. flagValues must carry the six base flags
// populated by flags.GetFlags (sort-by, sort-desc, delete, ...).
func (a *AWSCommand) runOrphanPipeline[I, R any](
    ctx context.Context,
    flagValues *map[string]any,
    spec OrphanPipeline[I, R],
) error {
    rootCtx := ctx

    collectDeletes := (*flagValues)["delete"].(bool) && spec.Delete != nil

    itemChan := make(chan I, 50)
    resultChan := make(chan R, 50)

    g, egCtx := errgroup.WithContext(ctx)

    // Producer: drains spec.List into itemChan.
    g.Go(func() error {
        defer close(itemChan)
        return spec.List(egCtx, func(item I) error {
            select {
            case itemChan <- item:
                return nil
            case <-egCtx.Done():
                return egCtx.Err()
            }
        })
    })

    // Workers.
    for range NumGoroutines {
        g.Go(func() error {
            for item := range itemChan {
                if err := egCtx.Err(); err != nil {
                    return err
                }
                r, err := spec.Process(egCtx, item)
                if err != nil {
                    return err
                }
                if r == nil {
                    continue
                }
                select {
                case resultChan <- *r:
                case <-egCtx.Done():
                    return egCtx.Err()
                }
            }
            return nil
        })
    }

    // Collector: streams rows as they arrive, keeps a slice for delete.
    collected := make([]R, 0)
    collectorDone := make(chan struct{})
    go func() {
        defer close(collectorDone)
        stream := printer.NewStreamTable(a.Output, true, spec.Headers)
        stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
        defer stream.Close()
        for r := range resultChan {
            stream.WriteRow(spec.ToRow(r)...)
            collected = append(collected, r)
        }
        if spec.Finalize != nil {
            if row := spec.Finalize(collected); row != nil {
                stream.WriteRow(row...)
            }
        }
    }()

    err := g.Wait()
    close(resultChan)
    <-collectorDone
    if err != nil {
        return err
    }

    if !collectDeletes || len(collected) == 0 {
        return nil
    }

    confirm, cErr := confirmDelete(a.Prompter, a.Logger)
    if cErr != nil {
        return cErr
    }
    if !confirm {
        return nil
    }
    for _, r := range collected {
        if err := spec.Delete(rootCtx, r); err != nil {
            return err
        }
    }
    return nil
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./cmd/ -run TestRunOrphanPipeline -v`
Expected: 4 tests PASS (`TestRunOrphanPipeline_HappyPath_StreamsAllRows`, `TestRunOrphanPipeline_DeleteCalledWhenConfirmed`, `TestRunOrphanPipeline_DeleteNotCalledWhenFlagFalse`, `TestRunOrphanPipeline_ProcessNilSkipsItem`).

- [ ] **Step 5: Run the full build / vet / test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success across the board, runner test still passes.

- [ ] **Step 6: Commit**

```bash
git add cmd/pipeline.go cmd/pipeline_test.go
git commit -m "$(cat <<'EOF'
refactor(cmd): add runOrphanPipeline shared pipeline helper

Introduces OrphanPipeline[I, R] + runOrphanPipeline to absorb the
errgroup producer/workers/collector ceremony shared by 15 of 25
command files. Delete-confirmation + rootCtx preservation move into
the helper. Per-command variation is declared via the spec struct's
List/Process/ToRow callbacks plus optional Finalize (e.g. volumes'
Total row) and Delete (the confirm-and-loop phase). Tests cover
happy-path streaming, delete-on/off paths, and Process returning nil
to skip an item.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Conversion recipe (applied in Tasks 2–6)

Each command conversion follows the same mechanical transformation:

1. **Replace `executeX`'s body** with a single `return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[I, R]{ ... })` call, inlining the command-specific List/Process/ToRow/Finalize/Delete closures.
2. **Remove** the command's errgroup-and-channels ceremony: the two channels, `g, egCtx := errgroup.WithContext(ctx)`, the producer `g.Go`, the worker `g.Go`, the collector `go func()`, the `g.Wait()` + close-channel + wait-for-collector block, and the old confirm-and-delete loop.
3. **Keep** command-local types (`orphanLambdaFunction`, `orphanECSCluster`, etc.), enrichment helpers (`checkLambdaOrphan`, `abortMultipartUploads`, `getVPCName`, etc.), and the `init()` flag registration. `Process` / `Delete` closures call these helpers.
4. **Remove** `delete<Name>` one-shot wrappers when they become a single SDK call — inline the SDK call into the `Delete` closure. Keep them if they wrap non-trivial logic (e.g. s3buckets' `abortMultipartUploads`, which loops over uploads).
5. **Run** `goimports -w cmd/<file>.go` to prune unused imports (typically `errgroup`, sometimes `time` where a `time.Sleep` collector-race hack existed).
6. **Remove** the per-file `rootCtx := ctx` line — `runOrphanPipeline` now owns it.
7. **Remove** any `a.Logger.LogError(...)` wrapping the overall processing failure — the helper returns the error and Cobra handles the user-facing surface, matching the refactor-A convention.

Per-batch verification (mandatory before commit):
- `go build ./...` clean
- `go vet ./...` clean
- `go test ./...` clean
- `go run . <cmd> --help` for at least one command in the batch prints the expected flags

---

## Task 2: Pilot — convert `cmd/volumes.go`

**Why:** Volumes is the richest spec (both `Finalize` for the Total row and `Delete` are exercised) and is the pilot walking-skeleton for the conversion recipe. If the pattern works here, Tasks 3-6 follow mechanically.

**Files:** Modify `cmd/volumes.go`.

### Steps

- [ ] **Step 1: Replace `executeVolumes` with the spec-driven version**

Edit `cmd/volumes.go`. Replace lines 61-196 (the `executeVolumes` method) with the following. Keep everything else (copyright header, imports except `errgroup` and `table`, `volumesCmd` var, `volumeWithTags`/`volumeResult` types, `init()`, and the `DescribeVolumes`/`handleVolume` helpers) unchanged for now — we'll prune unused helpers in Step 2.

```go
func (v *AWSCommand) executeVolumes(ctx context.Context, flagValues *map[string]any) error {
    filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))

    return v.runOrphanPipeline(ctx, flagValues, OrphanPipeline[types.Volume, volumePipelineResult]{
        Headers: []string{"Name", "Volume ID", "Snapshot ID", "Size"},
        List: func(ctx context.Context, emit func(types.Volume) error) error {
            filters := []types.Filter{
                {Name: aws.String("status"), Values: []string{"available"}},
            }
            if filterByName != "" {
                filters = append(filters, types.Filter{
                    Name:   aws.String("tag:Name"),
                    Values: []string{filterByName},
                })
            }
            p := ec2.NewDescribeVolumesPaginator(v.AWSClient.EC2, &ec2.DescribeVolumesInput{Filters: filters})
            for p.HasMorePages() {
                page, err := p.NextPage(ctx)
                if err != nil {
                    return err
                }
                for _, item := range page.Volumes {
                    if err := emit(item); err != nil {
                        return err
                    }
                }
            }
            return nil
        },
        Process: func(_ context.Context, vol types.Volume) (*volumePipelineResult, error) {
            name := "-"
            for _, t := range vol.Tags {
                if aws.ToString(t.Key) == "Name" && t.Value != nil {
                    name = *t.Value
                    break
                }
            }
            return &volumePipelineResult{
                name:       name,
                id:         aws.ToString(vol.VolumeId),
                snapshotID: aws.ToString(vol.SnapshotId),
                size:       aws.ToInt32(vol.Size),
            }, nil
        },
        ToRow: func(r volumePipelineResult) []any {
            return []any{r.name, r.id, r.snapshotID, r.size}
        },
        Finalize: func(results []volumePipelineResult) []any {
            var total int32
            for _, r := range results {
                total += r.size
            }
            return []any{"Total", "", "", total}
        },
        Delete: func(ctx context.Context, r volumePipelineResult) error {
            v.Logger.LogInfo("Deleting Volume", map[string]any{"VolumeId": r.id})
            _, err := v.AWSClient.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(r.id)})
            return err
        },
    })
}
```

- [ ] **Step 2: Add the `volumePipelineResult` type and remove now-unused helpers**

In `cmd/volumes.go`, replace the existing `volumeResult` struct (lines 39-42) with:

```go
type volumePipelineResult struct {
    name, id, snapshotID string
    size                 int32
}
```

Delete the `volumeWithTags` struct (lines 34-37), the `DescribeVolumes` method on `*AWSCommand` (lines 202-231), and the `handleVolume` function (lines 233-245). None is referenced anymore.

- [ ] **Step 3: Clean imports**

Run: `goimports -w cmd/volumes.go`

Expected removed imports: `"golang.org/x/sync/errgroup"`, `"github.com/jedib0t/go-pretty/v6/table"`, `"github.com/pincher95/cor/pkg/utils"`. Kept: `"context"`, `"github.com/aws/aws-sdk-go-v2/aws"`, `"github.com/aws/aws-sdk-go-v2/service/ec2"`, `"github.com/aws/aws-sdk-go-v2/service/ec2/types"`, `"github.com/pincher95/cor/pkg/handlers/flags"`, `handlers "github.com/pincher95/cor/pkg/handlers/aws"`, `"github.com/spf13/cobra"`.

- [ ] **Step 4: Build / vet / test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 5: Smoke test**

Run: `go run . volumes --help`
Expected: usage block with `--filter-by-name` plus the global persistent flags. No errors. The delete path is not smoke-tested here (no AWS account assumed); the runner test covers the confirm-and-delete logic already.

- [ ] **Step 6: Commit**

```bash
git add cmd/volumes.go
git commit -m "$(cat <<'EOF'
refactor(cmd/volumes): convert to runOrphanPipeline

Collapses executeVolumes from ~135 lines of errgroup/channels/collector
ceremony to a single spec-literal call into runOrphanPipeline. Total-row
writing migrates to the spec's Finalize hook. Delete moves into a Delete
closure that calls DeleteVolume directly. Drops the unused
volumeWithTags struct, DescribeVolumes helper, and handleVolume function.
Behavior unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Batch 1 — natgateways, ecs, targetgroups (simple pipelines)

**Why:** Three commands with single-paginator producers and single-SDK-call deletes. Easiest batch after the pilot; validates the recipe on multiple files at once.

**Files:** Modify `cmd/natgateways.go`, `cmd/ecs.go`, `cmd/targetgroups.go`.

### Steps

- [ ] **Step 1: Convert `cmd/natgateways.go`**

Replace the `executeNatGateways` method body (lines 61-187) with:

```go
func (c *AWSCommand) executeNatGateways(ctx context.Context, flagValues *map[string]any) error {
    stateFilter := (*flagValues)["filter-by-state"].(string)

    return c.runOrphanPipeline(ctx, flagValues, OrphanPipeline[types.NatGateway, natGatewayInfo]{
        Headers: []string{"Name", "ID", "State", "VPC", "Subnet", "Created"},
        List: func(ctx context.Context, emit func(types.NatGateway) error) error {
            filters := []types.Filter{}
            if stateFilter != "" {
                filters = append(filters, types.Filter{
                    Name:   aws.String("state"),
                    Values: []string{stateFilter},
                })
            }
            p := ec2.NewDescribeNatGatewaysPaginator(c.AWSClient.EC2, &ec2.DescribeNatGatewaysInput{Filter: filters})
            for p.HasMorePages() {
                page, err := p.NextPage(ctx)
                if err != nil {
                    return err
                }
                for _, ng := range page.NatGateways {
                    if err := emit(ng); err != nil {
                        return err
                    }
                }
            }
            return nil
        },
        Process: func(_ context.Context, ng types.NatGateway) (*natGatewayInfo, error) {
            name := "-"
            for _, tag := range ng.Tags {
                if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
                    name = *tag.Value
                    break
                }
            }
            return &natGatewayInfo{
                Name:    name,
                ID:      aws.ToString(ng.NatGatewayId),
                State:   string(ng.State),
                VpcID:   aws.ToString(ng.VpcId),
                Subnet:  aws.ToString(ng.SubnetId),
                Created: ng.CreateTime.String(),
            }, nil
        },
        ToRow: func(r natGatewayInfo) []any {
            return []any{r.Name, r.ID, r.State, r.VpcID, r.Subnet, r.Created}
        },
        Delete: func(ctx context.Context, r natGatewayInfo) error {
            c.Logger.LogInfo("Deleting NAT Gateway", map[string]any{"ID": r.ID, "Name": r.Name})
            _, err := c.AWSClient.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(r.ID)})
            return err
        },
    })
}
```

Run `goimports -w cmd/natgateways.go`. Expected removed: `"golang.org/x/sync/errgroup"`.

- [ ] **Step 2: Convert `cmd/ecs.go`**

Replace the `executeECS` method body (lines 70-182) with:

```go
func (a *AWSCommand) executeECS(ctx context.Context, flagValues *map[string]any) error {
    return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[string, orphanECSCluster]{
        Headers: []string{"Cluster Name", "Status", "Registered Tasks", "Running Tasks", "Services", "Reason"},
        List: func(ctx context.Context, emit func(string) error) error {
            p := ecs.NewListClustersPaginator(a.AWSClient.ECS, &ecs.ListClustersInput{})
            for p.HasMorePages() {
                page, err := p.NextPage(ctx)
                if err != nil {
                    return err
                }
                for _, clusterARN := range page.ClusterArns {
                    if err := emit(clusterARN); err != nil {
                        return err
                    }
                }
            }
            return nil
        },
        Process: func(ctx context.Context, clusterARN string) (*orphanECSCluster, error) {
            return a.checkECSOrphan(ctx, clusterARN)
        },
        ToRow: func(r orphanECSCluster) []any {
            return []any{r.ClusterName, r.Status, r.RegisteredTasks, r.RunningTasks, r.ServicesCount, r.Reason}
        },
        Delete: func(ctx context.Context, r orphanECSCluster) error {
            a.Logger.LogInfo("Deleting ECS cluster", map[string]any{"cluster": r.ClusterName})
            return a.deleteECSCluster(ctx, r.ClusterARN)
        },
    })
}
```

Keep `checkECSOrphan` and `deleteECSCluster` helpers as-is. Run `goimports -w cmd/ecs.go`. Expected removed: `"fmt"`, `"time"`, `"golang.org/x/sync/errgroup"`, `"github.com/jedib0t/go-pretty/v6/table"`.

- [ ] **Step 3: Convert `cmd/targetgroups.go`**

Define a command-local result type for target groups, then replace `executeTargetGroups` (lines 60-204). In `cmd/targetgroups.go`, add this struct near the top (after imports, before `targetgroupsCmd`):

```go
type orphanTargetGroup struct {
    name, arn, targetType, protocol, vpcID string
    port                                   int32
    attached                               int
}
```

Replace the `executeTargetGroups` method body with:

```go
func (t *AWSCommand) executeTargetGroups(ctx context.Context, flagValues *map[string]any) error {
    filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
    includeAttached := (*flagValues)["include-attached"].(bool)

    return t.runOrphanPipeline(ctx, flagValues, OrphanPipeline[elbtypes.TargetGroup, orphanTargetGroup]{
        Headers: []string{"TargetGroup Name", "TargetGroup ARN", "TargetType", "Protocol", "Port", "VPC ID", "Attached LBs"},
        List: func(ctx context.Context, emit func(elbtypes.TargetGroup) error) error {
            p := elasticloadbalancingv2.NewDescribeTargetGroupsPaginator(t.AWSClient.ELB, &elasticloadbalancingv2.DescribeTargetGroupsInput{})
            for p.HasMorePages() {
                page, err := p.NextPage(ctx)
                if err != nil {
                    return err
                }
                for _, tg := range page.TargetGroups {
                    if err := emit(tg); err != nil {
                        return err
                    }
                }
            }
            return nil
        },
        Process: func(_ context.Context, tg elbtypes.TargetGroup) (*orphanTargetGroup, error) {
            name := aws.ToString(tg.TargetGroupName)
            if filterByName != "" && !strings.Contains(name, filterByName) {
                return nil, nil
            }
            attachedCount := 0
            if tg.LoadBalancerArns != nil {
                attachedCount = len(tg.LoadBalancerArns)
            }
            if !includeAttached && attachedCount > 0 {
                return nil, nil
            }
            arn := aws.ToString(tg.TargetGroupArn)
            if arn == "" {
                return nil, nil
            }
            vpcID := aws.ToString(tg.VpcId)
            if vpcID == "" {
                vpcID = "-"
            }
            proto := string(tg.Protocol)
            if proto == "" {
                proto = "-"
            }
            port := int32(0)
            if tg.Port != nil {
                port = *tg.Port
            }
            tgType := string(tg.TargetType)
            if tgType == "" {
                tgType = "-"
            }
            return &orphanTargetGroup{
                name:       name,
                arn:        arn,
                targetType: tgType,
                protocol:   proto,
                vpcID:      vpcID,
                port:       port,
                attached:   attachedCount,
            }, nil
        },
        ToRow: func(r orphanTargetGroup) []any {
            return []any{r.name, r.arn, r.targetType, r.protocol, r.port, r.vpcID, r.attached}
        },
        Delete: func(ctx context.Context, r orphanTargetGroup) error {
            if r.attached > 0 || r.arn == "" || r.arn == "-" {
                return nil // skip — Process already filtered, but belt-and-suspenders
            }
            t.Logger.LogInfo("Deleting target group", map[string]any{"TargetGroupArn": r.arn})
            _, err := t.AWSClient.ELB.DeleteTargetGroup(ctx, &elasticloadbalancingv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(r.arn)})
            return err
        },
    })
}
```

Run `goimports -w cmd/targetgroups.go`. Expected removed: `"golang.org/x/sync/errgroup"`, `"github.com/jedib0t/go-pretty/v6/table"`.

- [ ] **Step 4: Build / vet / test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 5: Smoke test**

Run: `go run . natgateways --help` → expect `--filter-by-state`.
Run: `go run . targetgroups --help` → expect `--filter-by-name`, `--include-attached`.

- [ ] **Step 6: Commit**

```bash
git add cmd/natgateways.go cmd/ecs.go cmd/targetgroups.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert natgateways/ecs/targetgroups to runOrphanPipeline

Each command's executeX collapses to a single runOrphanPipeline call
with its List/Process/ToRow/Delete spec inlined. Enrichment helpers
(checkECSOrphan, deleteECSCluster) stay intact. Adds orphanTargetGroup
result type to give targetgroups a typed row shape. Drops now-unused
errgroup/table/fmt/time imports per file. Behavior unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Batch 2 — enis, elasticaddresses

**Why:** Two commands with more filter/enrichment complexity than Batch 1. ENIs has 15 flags, OR-logic listing (by ID or name), and VPC/subnet/SG name enrichment with caching. Elasticaddresses builds its delete input from either an AllocationId (VPC EIP) or a PublicIp (EC2-Classic).

**Files:** Modify `cmd/enis.go`, `cmd/elasticaddresses.go`.

### Steps

- [ ] **Step 1: Convert `cmd/elasticaddresses.go`**

Add a command-local result type, then replace `executeElasticIPs` (lines 59-211) with the new shape. Keep `describeAddresses` (since it handles the non-paginated DescribeAddresses call with panic recovery) — wrap its single emission in `List`.

Near the top of `cmd/elasticaddresses.go`, replace the `addressWithTags` struct (lines 33-36) with:

```go
type orphanElasticIP struct {
    name, allocationID, publicIP, associationID, networkInterfaceID string
}
```

Replace `executeElasticIPs` body with:

```go
func (a *AWSCommand) executeElasticIPs(ctx context.Context, flagValues *map[string]any) error {
    filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))

    return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[types.Address, orphanElasticIP]{
        Headers: []string{"Name", "Allocation ID", "Allocated Public address", "Association ID", "Network interface ID"},
        List: func(ctx context.Context, emit func(types.Address) error) error {
            filters := []types.Filter{}
            if filterByName != "" {
                filters = append(filters, types.Filter{
                    Name:   aws.String("tag:Name"),
                    Values: []string{filterByName},
                })
            }
            out, err := a.AWSClient.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: filters})
            if err != nil {
                return err
            }
            for _, addr := range out.Addresses {
                if err := emit(addr); err != nil {
                    return err
                }
            }
            return nil
        },
        Process: func(_ context.Context, addr types.Address) (*orphanElasticIP, error) {
            // Orphan only if neither associated nor attached to an instance.
            if addr.AssociationId != nil || addr.InstanceId != nil {
                return nil, nil
            }
            name := "-"
            if nameTag, ok := utils.TagsToMap(addr.Tags)["Name"]; ok && nameTag.Value != nil {
                name = *nameTag.Value
            }
            return &orphanElasticIP{
                name:               name,
                allocationID:       stringOrDash(addr.AllocationId),
                publicIP:           stringOrDash(addr.PublicIp),
                associationID:      stringOrDash(addr.AssociationId),
                networkInterfaceID: stringOrDash(addr.NetworkInterfaceId),
            }, nil
        },
        ToRow: func(r orphanElasticIP) []any {
            return []any{r.name, r.allocationID, r.publicIP, r.associationID, r.networkInterfaceID}
        },
        Delete: func(ctx context.Context, r orphanElasticIP) error {
            input := ec2.ReleaseAddressInput{}
            if r.allocationID != "" && r.allocationID != "-" {
                input.AllocationId = aws.String(r.allocationID)
            } else if r.publicIP != "" && r.publicIP != "-" {
                input.PublicIp = aws.String(r.publicIP)
            } else {
                return nil // nothing to release
            }
            a.Logger.LogInfo("Releasing Elastic IP", map[string]any{
                "AllocationId": aws.ToString(input.AllocationId),
                "PublicIp":     aws.ToString(input.PublicIp),
            })
            _, err := a.AWSClient.EC2.ReleaseAddress(ctx, &input)
            return err
        },
    })
}

// stringOrDash returns the dereferenced value of p, or "-" if p is nil or empty.
func stringOrDash(p *string) string {
    if p == nil {
        return "-"
    }
    s := *p
    if s == "" {
        return "-"
    }
    return s
}
```

Delete the `describeAddresses` method (lines 213-242) and the commented-out `handleElasticIP` block (lines 244-255). Run `goimports -w cmd/elasticaddresses.go`. Expected removed: `"golang.org/x/sync/errgroup"`, `"github.com/jedib0t/go-pretty/v6/table"`.

- [ ] **Step 2: Convert `cmd/enis.go` — add command-local result type**

Near the top of `cmd/enis.go`, between the existing `import` block and the `enisCmd` var, add:

```go
type orphanENI struct {
    name               string
    id                 string
    interfaceType      string
    status             string
    requesterManaged   bool
    description        string
    vpcDisplay         string
    subnetDisplay      string
    privateIP          string
    securityGroups     string
}
```

- [ ] **Step 3: Replace `executeENIs` body with the spec-driven version**

**IMPORTANT for the implementer:** ENIs' pre-refactor producer goroutine has nontrivial OR-logic (DescribeNetworkInterfaces by ID *or* by tag:Name with dedup via a `seen` map) and a cluster of filter-building code that's tightly coupled with the listing calls. Do not restructure that logic during this task — treat it as a verbatim move into a `List` closure. Do the same for the worker's row-formatting block (VPC/subnet/SG name enrichment + "ID (Name)" display formatting) — move it into `Process` + `ToRow` without changes to behavior.

Mechanical steps:

1. **Move filter parsing up-front.** The old `executeENIs` reads flags (both primary and backwards-compat aliases) inside its producer/worker goroutines. Move every flag read to the very top of the new `executeENIs` body so the values are closed over by both `List` and `Process` cleanly. The helpers for that reading (`getFlagString`, `mergeCSV`, `splitCSV`, `normalizeFilterValue`) already exist — do not rewrite them.

2. **Move the producer block into the `List` closure verbatim.** Everything the old `g.Go(func() error { ... })` producer did — the filter-building, the one-or-two paginator passes, the dedup via the `seen` map, the per-page item loop — lives inside the `List` closure. Change only the `eniChan <- ni` send to `if err := emit(ni); err != nil { return err }`. Keep `seen` scoped to the closure.

3. **Move the worker's per-item enrichment into `Process`.** The old worker computes: Name tag lookup, VPC name via `e.getVPCName` (cached), subnet name via `e.getSubnetName` (cached), SG names via `e.ensureSGNames` (cached), display strings via `formatIDAndName` (existing helper), and the SG list formatting (in the old code this is an inline loop producing a newline-joined "id (Name)" string). Put the enrichment in `Process`; it returns the populated `orphanENI` or `nil, nil` if `eniID == ""`.

4. **Move the SG-list formatting into a helper.** Extract the inline "id (Name)" joining code the old worker uses into `formatSGList(ids []string, names map[string]string) string` (or whatever signature matches how `ensureSGNames` returns results — match the existing code's expectations). `ToRow` calls `formatSGList` if needed, or stores its result inside `Process` and passes through as a preformatted string field on `orphanENI`. Both are acceptable; the latter keeps `ToRow` trivial.

5. **Delete the old ENI-skip-logic for requester-managed.** The old code filtered RequesterManaged-true items OUT of the delete list at collector time. Move this to the `Delete` closure: if `r.requesterManaged` is true, skip (return nil). This matches current behavior.

Replace the existing `executeENIs` method with this spec-driven shape (fill in the bodies of List/Process from Steps 2 and 3 above — the plan's code sketch shows the overall shape):

```go
func (e *AWSCommand) executeENIs(ctx context.Context, flagValues *map[string]any) error {
    // Read all flags up front; each filter is then closed over by the List/Process closures.
    filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
    filterByENIs := splitCSV(mergeCSV(getFlagString(flagValues, "filter-by-enis"), getFlagString(flagValues, "filter-by-id-or-name")))
    filterByVPC := splitCSV(mergeCSV(getFlagString(flagValues, "filter-by-vpc"), getFlagString(flagValues, "filter-by-vpc-id")))
    filterBySubnet := splitCSV(mergeCSV(getFlagString(flagValues, "filter-by-subnet"), getFlagString(flagValues, "filter-by-subnet-id")))
    filterBySG := splitCSV(mergeCSV(getFlagString(flagValues, "filter-by-sg"), getFlagString(flagValues, "filter-by-security-group-id")))
    filterByType := splitCSV(mergeCSV(getFlagString(flagValues, "filter-by-type"), getFlagString(flagValues, "filter-by-interface-type")))
    filterByDesc := splitCSV(mergeCSV(getFlagString(flagValues, "filter-by-desc"), getFlagString(flagValues, "filter-by-description")))
    filterByIP := splitCSV(mergeCSV(getFlagString(flagValues, "filter-by-ip"), getFlagString(flagValues, "filter-by-private-ip")))
    _ = filterByName
    _ = filterByENIs
    _ = filterByVPC
    _ = filterBySubnet
    _ = filterBySG
    _ = filterByType
    _ = filterByDesc
    _ = filterByIP
    // (These underscore-assignments are placeholders — remove them once
    // the List closure below actually references the filters. Kept here
    // as a reminder that every pre-refactor filter must be consumed.)

    return e.runOrphanPipeline(ctx, flagValues, OrphanPipeline[types.NetworkInterface, orphanENI]{
        Headers: []string{"Name", "ENI ID", "Type", "Status", "RequesterManaged", "Description", "VPC", "Subnet", "Private IP", "Security Groups"},
        List: func(ctx context.Context, emit func(types.NetworkInterface) error) error {
            // Paste the pre-refactor producer body here (filter-building,
            // DescribeNetworkInterfaces paginator loop, optional by-name
            // secondary paginator, dedup via `seen` map). Change
            // `eniChan <- ni` (including the selected-send form) to
            // `if err := emit(ni); err != nil { return err }`.
            return nil // replace with actual body
        },
        Process: func(ctx context.Context, ni types.NetworkInterface) (*orphanENI, error) {
            eniID := aws.ToString(ni.NetworkInterfaceId)
            if eniID == "" {
                return nil, nil
            }
            // Paste the pre-refactor worker body here (Name tag lookup,
            // getVPCName, getSubnetName, ensureSGNames, format strings).
            // Return an *orphanENI populated with all rows fields.
            return nil, nil // replace with actual body
        },
        ToRow: func(r orphanENI) []any {
            return []any{r.name, r.id, r.interfaceType, r.status, r.requesterManaged, r.description, r.vpcDisplay, r.subnetDisplay, r.privateIP, r.securityGroups}
        },
        Delete: func(ctx context.Context, r orphanENI) error {
            if r.requesterManaged {
                return nil // skip AWS-managed ENIs (matches pre-refactor skip logic)
            }
            e.Logger.LogInfo("Deleting ENI", map[string]any{"ENI": r.id})
            _, err := e.AWSClient.EC2.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(r.id)})
            return err
        },
    })
}
```

- [ ] **Step 4: Verify filter/enrichment behavior**

After Steps 1-3 above, the ENI output should match pre-refactor output exactly. Checks:

1. `go build ./...` clean (compiler catches missing fields on `orphanENI` or stale type references).
2. `go vet ./...` clean (catches typos in closure captures).
3. Diff the old and new `executeENIs` mentally: every flag the old code reads is read in the new one; every filter applied in the old producer is applied in the new `List`; every column in the old row appears in the new `ToRow`; the RequesterManaged skip-delete rule is in the new `Delete`. Anything missed will manifest as a behavior regression — worth a careful pass before commit.

- [ ] **Step 5: Clean up `cmd/enis.go`**

Run `goimports -w cmd/enis.go`. Expected removed: `"golang.org/x/sync/errgroup"`, `"github.com/jedib0t/go-pretty/v6/table"`. Imports retained: the existing ENI caching helpers need `sync`, which stays.

- [ ] **Step 6: Build / vet / test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 7: Smoke test**

Run: `go run . enis --help`
Expected: 8 visible flags (7 primary + `filter-by-name`), plus deprecated-but-hidden backwards-compat aliases acknowledged via `--help` flag listing (depending on how cobra registers hidden flags in this repo).

Run: `go run . elasticips --help`
Expected: `--filter-by-name` plus globals.

- [ ] **Step 8: Commit**

```bash
git add cmd/enis.go cmd/elasticaddresses.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert enis/elasticaddresses to runOrphanPipeline

ENIs: producer OR-listing and dedup move into listENIsWithFilters;
per-item enrichment (VPC/subnet/SG name lookups with caching) happens
inside Process. Filters are read up-front and closed over by List and
Process. Requester-managed skip-delete rule lives in the Delete closure.

ElasticIPs: the non-paginated DescribeAddresses fits in List as a
single call + loop. The single-step Release flow (either AllocationId
for VPC EIPs or PublicIp for EC2-Classic) lives in Delete.

Behavior unchanged on both.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Batch 3 — lambda, elasticache, opensearch, dynamodb (CloudWatch enrichment)

**Why:** Four commands whose `Process` is a heavy enrichment closure — each calls `GetMetricStatistics` on CloudWatch plus additional describe/list calls per item. The per-item AWS work stays in existing `check<Name>Orphan` helpers; the shape change is purely about absorbing the producer/collector ceremony.

**Files:** Modify `cmd/lambda.go`, `cmd/elasticache.go`, `cmd/opensearch.go`, `cmd/dynamodb.go`.

For **all four**, the recipe is:

1. Add (or adjust) a `ToRow` hook on the existing orphan result struct.
2. Rewrite `executeX` to `return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[ItemType, ResultStruct]{ ... })`.
3. List calls the existing paginator/list endpoint, emitting items.
4. Process calls the existing `check<Name>Orphan` helper unchanged (returns `(*Result, error)` — `nil, nil` skips).
5. ToRow mirrors the existing row-construction block from the old collector.
6. Delete calls the existing `delete<Name>` helper (or inlines the single SDK call).
7. Drop the old errgroup/channels/collector block.

### Steps

- [ ] **Step 1: Convert `cmd/lambda.go`**

Replace the `executeLambda` method body (lines 120-200+) with:

```go
func (a *AWSCommand) executeLambda(ctx context.Context, flagValues *map[string]any) error {
    daysSinceInvocation := int64((*flagValues)["days-since-invocation"].(int))
    minOldVersions := int32((*flagValues)["min-old-versions"].(int))

    return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[lambdatypes.FunctionConfiguration, orphanLambdaFunction]{
        Headers: []string{"Function Name", "Runtime", "Memory", "Last Modified", "Days Since Invocation", "Versions", "Provisioned Concurrency", "Reason"},
        List: func(ctx context.Context, emit func(lambdatypes.FunctionConfiguration) error) error {
            p := lambda.NewListFunctionsPaginator(a.AWSClient.Lambda, &lambda.ListFunctionsInput{})
            for p.HasMorePages() {
                page, err := p.NextPage(ctx)
                if err != nil {
                    return err
                }
                for _, fn := range page.Functions {
                    if err := emit(fn); err != nil {
                        return err
                    }
                }
            }
            return nil
        },
        Process: func(ctx context.Context, fn lambdatypes.FunctionConfiguration) (*orphanLambdaFunction, error) {
            return a.checkLambdaOrphan(ctx, &fn, daysSinceInvocation, minOldVersions)
        },
        ToRow: func(r orphanLambdaFunction) []any {
            return []any{
                r.Name,
                r.Runtime,
                fmt.Sprintf("%d MB", r.MemorySize),
                r.LastModified,
                fmt.Sprintf("%d days", r.LastInvocationDays),
                r.Versions,
                r.ProvisionedConcurrency,
                r.Reason,
            }
        },
        Delete: func(ctx context.Context, r orphanLambdaFunction) error {
            a.Logger.LogInfo("Deleting Lambda function", map[string]any{"function": r.Name})
            return a.deleteLambdaFunction(ctx, r.Name)
        },
    })
}
```

**Important:** `checkLambdaOrphan` currently takes `*lambdatypes.FunctionConfiguration`. The pipeline emits by value; `Process` takes `&fn` to keep the helper signature unchanged.

Keep `checkLambdaOrphan` and `deleteLambdaFunction` unchanged. Run `goimports -w cmd/lambda.go`. Expected removed: `"time"` (if only used by the old `time.Sleep(100ms)` hack), `"golang.org/x/sync/errgroup"`, `"github.com/jedib0t/go-pretty/v6/table"`.

- [ ] **Step 2: Convert `cmd/elasticache.go`**

Replace `executeElastiCache` body with:

```go
func (a *AWSCommand) executeElastiCache(ctx context.Context, flagValues *map[string]any) error {
    hoursZeroConnections := int64((*flagValues)["hours-zero-connections"].(int))

    return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[elasticachetypes.CacheCluster, orphanElastiCacheCluster]{
        Headers: []string{"Cluster ID", "Engine", "Node Type", "Nodes", "Status", "Created", "Days Since Activity", "Reason"},
        List: func(ctx context.Context, emit func(elasticachetypes.CacheCluster) error) error {
            p := elasticache.NewDescribeCacheClustersPaginator(a.AWSClient.ElastiCache, &elasticache.DescribeCacheClustersInput{})
            for p.HasMorePages() {
                page, err := p.NextPage(ctx)
                if err != nil {
                    return err
                }
                for _, c := range page.CacheClusters {
                    if err := emit(c); err != nil {
                        return err
                    }
                }
            }
            return nil
        },
        Process: func(ctx context.Context, c elasticachetypes.CacheCluster) (*orphanElastiCacheCluster, error) {
            return a.checkElastiCacheOrphan(ctx, &c, hoursZeroConnections)
        },
        ToRow: func(r orphanElastiCacheCluster) []any {
            return []any{
                r.ClusterID,
                r.Engine,
                r.CacheNodeType,
                r.NumNodes,
                r.Status,
                r.CreatedDate,
                fmt.Sprintf("%d days", r.DaysSinceActivity),
                r.Reason,
            }
        },
        Delete: func(ctx context.Context, r orphanElastiCacheCluster) error {
            a.Logger.LogInfo("Deleting ElastiCache cluster", map[string]any{"ClusterId": r.ClusterID})
            return a.deleteElastiCacheCluster(ctx, r.ClusterID)
        },
    })
}
```

Keep `checkElastiCacheOrphan` (may need its signature adjusted to take `*CacheCluster` + `hoursZeroConnections` if it doesn't already — check the existing signature and match). Keep `deleteElastiCacheCluster`. Run `goimports`.

- [ ] **Step 3: Convert `cmd/opensearch.go`**

OpenSearch's List uses `ListDomainNames` (direct, not a paginator) and emits domain name strings. Workers then call `DescribeDomain` inside `checkOpenSearchOrphan`.

Replace `executeOpenSearch` body with:

```go
func (a *AWSCommand) executeOpenSearch(ctx context.Context, flagValues *map[string]any) error {
    daysNoIndexing := int64((*flagValues)["days-no-indexing"].(int))
    hoursNoSearches := int64((*flagValues)["hours-no-searches"].(int))

    return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[string, orphanOpenSearchDomain]{
        Headers: []string{"Domain Name", "Version", "Instance Type", "Instances", "Storage", "Created", "Days Since Activity", "Reason"},
        List: func(ctx context.Context, emit func(string) error) error {
            out, err := a.AWSClient.OpenSearch.ListDomainNames(ctx, &opensearch.ListDomainNamesInput{})
            if err != nil {
                return err
            }
            for _, d := range out.DomainNames {
                if err := emit(aws.ToString(d.DomainName)); err != nil {
                    return err
                }
            }
            return nil
        },
        Process: func(ctx context.Context, domainName string) (*orphanOpenSearchDomain, error) {
            return a.checkOpenSearchOrphan(ctx, domainName, daysNoIndexing, hoursNoSearches)
        },
        ToRow: func(r orphanOpenSearchDomain) []any {
            return []any{
                r.DomainName,
                r.EngineVersion,
                r.InstanceType,
                r.InstanceCount,
                fmt.Sprintf("%d GB", r.StorageSize),
                r.Created,
                fmt.Sprintf("%d days", r.DaysSinceActivity),
                r.Reason,
            }
        },
        Delete: func(ctx context.Context, r orphanOpenSearchDomain) error {
            a.Logger.LogInfo("Deleting OpenSearch domain", map[string]any{"DomainName": r.DomainName})
            return a.deleteOpenSearchDomain(ctx, r.DomainName)
        },
    })
}
```

Keep `checkOpenSearchOrphan` and `deleteOpenSearchDomain`. Run `goimports`.

- [ ] **Step 4: Convert `cmd/dynamodb.go`**

DynamoDB's List uses `ListTablesPaginator` and emits table-name strings. `checkDynamoDBOrphan` calls `DescribeTable` + CloudWatch metrics.

Replace `executeDynamoDB` body with:

```go
func (a *AWSCommand) executeDynamoDB(ctx context.Context, flagValues *map[string]any) error {
    daysNoActivity := int64((*flagValues)["days-no-activity"].(int))

    return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[string, orphanDynamoDBTable]{
        Headers: []string{"Table Name", "Billing Mode", "Status", "Items", "Size", "Read Capacity", "Write Capacity", "Days No Activity", "Reason"},
        List: func(ctx context.Context, emit func(string) error) error {
            p := dynamodb.NewListTablesPaginator(a.AWSClient.DynamoDB, &dynamodb.ListTablesInput{})
            for p.HasMorePages() {
                page, err := p.NextPage(ctx)
                if err != nil {
                    return err
                }
                for _, name := range page.TableNames {
                    if err := emit(name); err != nil {
                        return err
                    }
                }
            }
            return nil
        },
        Process: func(ctx context.Context, tableName string) (*orphanDynamoDBTable, error) {
            return a.checkDynamoDBOrphan(ctx, tableName, daysNoActivity)
        },
        ToRow: func(r orphanDynamoDBTable) []any {
            return []any{
                r.TableName,
                r.BillingMode,
                r.TableStatus,
                r.ItemCount,
                fmt.Sprintf("%.2f GB", float64(r.TableSize)/(1024*1024*1024)),
                r.ReadCapacity,
                r.WriteCapacity,
                fmt.Sprintf("%d days", r.DaysSinceActivity),
                r.Reason,
            }
        },
        Delete: func(ctx context.Context, r orphanDynamoDBTable) error {
            a.Logger.LogInfo("Deleting DynamoDB table", map[string]any{"TableName": r.TableName})
            return a.deleteDynamoDBTable(ctx, r.TableName)
        },
    })
}
```

Keep `checkDynamoDBOrphan` and `deleteDynamoDBTable`. Run `goimports`.

- [ ] **Step 5: Build / vet / test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

If any `check<Name>Orphan` signature mismatch appears (e.g. the helper took different arg shapes than the new Process closure expects), adjust the helper to match the new call — but preserve behavior. A signature-adjust commit before this batch's main commit is acceptable if the diff gets too noisy.

- [ ] **Step 6: Smoke test**

Run each and confirm flags print:
- `go run . lambda --help`
- `go run . elasticache --help`
- `go run . opensearch --help`
- `go run . dynamodb --help`

- [ ] **Step 7: Commit**

```bash
git add cmd/lambda.go cmd/elasticache.go cmd/opensearch.go cmd/dynamodb.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert lambda/elasticache/opensearch/dynamodb to runOrphanPipeline

Each command's executeX collapses to a runOrphanPipeline call. The
CloudWatch-enrichment helpers (checkLambdaOrphan, checkElastiCacheOrphan,
checkOpenSearchOrphan, checkDynamoDBOrphan) stay as the per-item Process
closure; delete helpers stay as the per-item Delete closure. Drops the
per-file errgroup/channels/collector ceremony and the time.Sleep race
hacks that some commands used to wait on the printer goroutine.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Batch 4 — s3buckets + final verification

**Why:** S3Buckets is the last conversion and has the most unusual shape (two-phase delete with regional clients). After it lands, all 11 target files use `runOrphanPipeline`. The second half of this task is the end-of-refactor verification pass.

**Files:** Modify `cmd/s3buckets.go`.

### Steps

- [ ] **Step 1: Convert `cmd/s3buckets.go`**

Replace `executeS3Buckets` body with:

```go
func (a *AWSCommand) executeS3Buckets(ctx context.Context, flagValues *map[string]any) error {
    checkLifecycle := (*flagValues)["check-lifecycle"].(bool)

    return a.runOrphanPipeline(ctx, flagValues, OrphanPipeline[s3types.Bucket, orphanS3Bucket]{
        Headers: []string{"Bucket Name", "Region", "Created", "Empty", "Incomplete Uploads", "Has Lifecycle", "Reason"},
        List: func(ctx context.Context, emit func(s3types.Bucket) error) error {
            out, err := a.AWSClient.S3.ListBuckets(ctx, &s3.ListBucketsInput{})
            if err != nil {
                return err
            }
            for _, b := range out.Buckets {
                if err := emit(b); err != nil {
                    return err
                }
            }
            return nil
        },
        Process: func(ctx context.Context, b s3types.Bucket) (*orphanS3Bucket, error) {
            return a.checkS3BucketOrphan(ctx, b, checkLifecycle)
        },
        ToRow: func(r orphanS3Bucket) []any {
            yesNo := func(b bool) string {
                if b {
                    return "Yes"
                }
                return "No"
            }
            return []any{
                r.BucketName,
                r.Region,
                r.CreationDate,
                yesNo(r.IsEmpty),
                r.IncompleteUploads,
                yesNo(r.HasLifecyclePolicy),
                r.Reason,
            }
        },
        Delete: func(ctx context.Context, r orphanS3Bucket) error {
            if r.IncompleteUploads > 0 {
                if err := a.abortMultipartUploads(ctx, r.BucketName, r.Region); err != nil {
                    a.Logger.LogError("Failed to abort multipart uploads", err, map[string]any{"bucket": r.BucketName}, false)
                    return err
                }
            }
            if !r.IsEmpty {
                a.Logger.LogInfo("Skipped non-empty S3 bucket", map[string]any{"bucket": r.BucketName})
                return nil
            }
            if err := a.deleteS3Bucket(ctx, r.BucketName, r.Region); err != nil {
                a.Logger.LogError("Failed to delete S3 bucket", err, map[string]any{"bucket": r.BucketName}, false)
                return err
            }
            a.Logger.LogInfo("Deleted S3 bucket", map[string]any{"bucket": r.BucketName})
            return nil
        },
    })
}
```

Keep `checkS3BucketOrphan`, `abortMultipartUploads`, and `deleteS3Bucket` — they are nontrivial regional-client constructors and stay as-is.

Run `goimports -w cmd/s3buckets.go`. Expected removed: `"time"` (if only used by the old `time.Sleep` hack), `"golang.org/x/sync/errgroup"`, `"github.com/jedib0t/go-pretty/v6/table"`, `"fmt"` (if only used for log-format strings in the old collector's "Found N" info log).

- [ ] **Step 2: Build / vet / test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 3: Smoke test**

Run: `go run . s3buckets --help`
Expected: `--check-lifecycle` plus globals.

- [ ] **Step 4: Final count — `runOrphanPipeline` usage**

Run: `grep -l "runOrphanPipeline" cmd/*.go | wc -l`
Expected: `11` (all 11 converted commands — does not count `pipeline.go` or `pipeline_test.go`).

Run: `grep -c "errgroup.WithContext" cmd/*.go | grep -v :0`
Expected: exactly 4 matches (one per remaining out-of-scope file): `cmd/rds.go`, `cmd/logs.go`, `cmd/images.go`, `cmd/elbv2.go`.

- [ ] **Step 5: Run full pre-commit**

Run: `pre-commit run --all-files`
Expected: all hooks pass (go-fmt, go-vet, goimports, golangci-lint, go-unit-tests, go-build, go-mod-tidy).

If any hook surfaces a fix the conversion missed, apply it and continue to Step 6.

- [ ] **Step 6: Measure LOC reduction**

Run: `wc -l cmd/*.go | tail -1`
Record the number. Compare to the pre-"B" baseline (cmd/ was 6,806 LOC after refactor A). Expected reduction: 400-500 LOC **excluding** the +~350 added by `pipeline.go` + `pipeline_test.go`. Net across refactor A + B: `cmd/` went from 7,597 (pre-A) to ~6,700 (post-B).

- [ ] **Step 7: Commit**

```bash
git add cmd/s3buckets.go
git commit -m "$(cat <<'EOF'
refactor(cmd/s3buckets): convert to runOrphanPipeline

The list + per-item enrichment (region-specific clients built inside
checkS3BucketOrphan) moves into List/Process. The two-phase delete
(abort incomplete uploads, then DeleteBucket if empty) lives entirely
in the Delete closure, which still hands region-specific context to
abortMultipartUploads and deleteS3Bucket unchanged. All 11 target
commands now use runOrphanPipeline; the out-of-scope four (rds, logs,
images, elbv2) remain custom per plan.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Definition of done

After Task 6 commits:

- `cmd/pipeline.go` and `cmd/pipeline_test.go` exist; the test passes.
- 11 of 15 pipeline-shaped commands route through `runOrphanPipeline` (greppable).
- The four out-of-scope commands (rds, logs, images, elbv2) still use their custom errgroup code — unchanged.
- `go build ./... && go vet ./... && go test ./...` clean.
- Pre-commit hooks clean.
- No runtime behavior change on any of the 11 converted commands.
