# `cmd/` Pipeline Abstraction Refactor — Design

**Status:** Design approved 2026-04-20. Implementation plan to follow.

This is the pre-agreed "B" step in the three-part sequence. "A" (shell runner refactor) merged earlier the same day. "C" (new commands, cross-cutting features) remains future work.

## Motivation

After refactor A, every `executeX` method still carries ~80-100 lines of near-identical ceremony to run the producer → workers → collector pipeline that AWS-listing commands share:

- Two buffered channels (items, results) with a cap of 50.
- An `errgroup.WithContext` wrapping one paginator-driven producer goroutine and `NumGoroutines=10` worker goroutines.
- A plain collector goroutine that drains results into a `printer.StreamTable`.
- Post-`Wait` channel cleanup plus a confirm-and-delete loop using the preserved `rootCtx` so errgroup cancellation on error doesn't block deletes.

15 of 25 commands use this shape. 11 of them are structurally identical enough to fit a single shared helper; the remaining 4 (rds, logs, images, elbv2) have enough variation that forcing them into a general abstraction would either distort the helper or require so many opt-in knobs that the ceremony saving gets eaten by configuration noise.

Collapsing the ceremony for the 11 standard commands is expected to save ~500 LOC and — more importantly — reduce each standard command to its actual business-logic content: which AWS API to paginate, how to decide if an item is orphaned, what columns to print, how to delete.

## Scope

### In scope — convert these 11 commands

volumes, enis, elasticaddresses, lambda, elasticache, opensearch, s3buckets, dynamodb, targetgroups, natgateways, ecs.

All are paginator-driven (or single-call list) + per-item process + optional delete. Enrichment during `Process` (e.g. lambda's CloudWatch invocation metrics, dynamodb's read/write activity lookup, s3buckets' regional client construction) lives entirely inside the per-item callback and doesn't need helper awareness.

### Out of scope (kept custom)

- **rds** — two conditional producers (instances + snapshots) feeding the same stream; picked by `--include-instances` / `--include-snapshots`. Abstracting it cleanly would either require the helper to accept a pre-built stream (breaking the one-pipeline-owns-its-stream contract) or force two separate tables in output (user-visible UX change). Not worth the complexity for one command.
- **logs** — no separate producer goroutine; inline pagination feeding workers with a single `g.Go`. Different skeleton from the standard shape.
- **images** — nested pagination inside workers (AMI lookup then cross-referencing snapshots / instances / launch templates). The "enrichment" step is itself a multi-paginator sub-pipeline.
- **elbv2** — three `g.Go` blocks; an extra listener-deletion pipeline that runs after the main LB scan. More stages than the standard shape.

### Out of scope — same deferrals as refactor A

- `flagValues *map[string]any` → typed struct migration.
- Receiver-name consistency across `executeX` methods.
- `AWSClientImpl` restructuring.
- Cleanups from refactor A reviews (dead EC2 in `targetgroups`, `rootCtx` comment consistency) — fold in only where a pipeline conversion naturally touches the file.

## Architecture

### New file: `cmd/pipeline.go`

```go
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

    // List paginates or lists the resource type. It must call emit
    // exactly once per item; emit handles the context-aware handoff
    // to the worker pool and returns an error that List must
    // propagate (stopping further enumeration).
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

### Per-command shape (after conversion)

`cmd/volumes.go` is the canonical example. Business logic stays in helper closures that call AWS SDK directly; the `Process` and `Delete` bodies do the real work, and the rest is just the spec literal.

```go
type volumeResult struct {
    name, id, snapshotID string
    size                 int32
}

func (v *AWSCommand) executeVolumes(ctx context.Context, flagValues *map[string]any) error {
    filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))

    return v.runOrphanPipeline(ctx, flagValues, OrphanPipeline[ec2types.Volume, volumeResult]{
        Headers: []string{"Name", "Volume ID", "Snapshot ID", "Size"},
        List: func(ctx context.Context, emit func(ec2types.Volume) error) error {
            filters := []ec2types.Filter{
                {Name: aws.String("status"), Values: []string{"available"}},
            }
            if filterByName != "" {
                filters = append(filters, ec2types.Filter{
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
        Process: func(_ context.Context, vol ec2types.Volume) (*volumeResult, error) {
            name := "-"
            for _, t := range vol.Tags {
                if aws.ToString(t.Key) == "Name" && t.Value != nil {
                    name = *t.Value
                    break
                }
            }
            return &volumeResult{
                name:       name,
                id:         aws.ToString(vol.VolumeId),
                snapshotID: aws.ToString(vol.SnapshotId),
                size:       aws.ToInt32(vol.Size),
            }, nil
        },
        ToRow: func(r volumeResult) []any {
            return []any{r.name, r.id, r.snapshotID, r.size}
        },
        Finalize: func(results []volumeResult) []any {
            var total int32
            for _, r := range results {
                total += r.size
            }
            return []any{"Total", "", "", total}
        },
        Delete: func(ctx context.Context, r volumeResult) error {
            _, err := v.AWSClient.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(r.id)})
            return err
        },
    })
}
```

Compare the current `executeVolumes` (~135 lines after refactor A) — this converges it to ~50 lines of meaningful configuration plus the spec literal framing.

### Per-command quirk handling

- **volumes** — `Finalize` returns the `Total` row.
- **s3buckets** — `Delete` does the existing two-step `abortMultipartUploads` + `deleteS3Bucket` using `a.CloudConfig` (added to `AWSCommand` in refactor A's Task 2).
- **elasticaddresses** — `Delete` does `DisassociateAddress` (if associated) then `ReleaseAddress`. Both live inside the one closure.
- **lambda / dynamodb / elasticache / opensearch** — per-item CloudWatch metric fetches and list-versions calls all happen inside `Process`. They already use `a.AWSClient.CloudWatch` and friends, which stay populated by the existing `BuildClients` wiring.
- **enis** — 15 flags drive complex filtering. The filter logic moves into `Process` (and into `List` where AWS-side filters exist). Behavior unchanged.

### `rootCtx` invariant preserved

The helper captures `rootCtx := ctx` before entering the errgroup, and passes `rootCtx` (not `egCtx`) to `spec.Delete`. This is the critical invariant from refactor A that prevented deletes from instantly failing with `context canceled` when a worker returned an error.

## Testing

One focused test in `cmd/pipeline_test.go`: `TestRunOrphanPipeline_HappyPath`. Constructs a fake spec where:

- `List` emits 3 synthetic items.
- `Process` returns one result per item.
- `ToRow` produces two columns.
- `Finalize` returns a totals row.
- `Delete` is set but `--delete` stays false.

Verifies:

- All 3 results arrive at the collector.
- The streamed output buffer contains 4 rows (3 data + 1 finalize).
- `Delete` was NOT called (since `--delete` is false).
- A second sub-test with `--delete=true` and a stubbed prompter approving: confirms `Delete` is called exactly 3 times, each receiving a non-nil context.

No real AWS calls. No tests added for individual commands — consistent with refactor A.

## Migration strategy

Six tasks, roughly parallel to refactor A's shape.

1. **Task 1 (TDD).** Add `cmd/pipeline.go` + `cmd/pipeline_test.go`. Test first, red phase, implement, green phase. Commit.
2. **Task 2 (pilot).** Convert `cmd/volumes.go`. Volumes is the richest spec — both `Finalize` (totals) and `Delete` exercised. Walking-skeleton check. Commit.
3. **Task 3 (batch 1).** Convert simple single-client commands: natgateways, ecs, targetgroups. Commit.
4. **Task 4 (batch 2).** Convert ENI + elasticaddresses (elasticaddresses' Delete is the two-step disassociate-then-release). Commit.
5. **Task 5 (batch 3).** Convert multi-client enrichment commands: lambda, elasticache, opensearch, dynamodb. Commit.
6. **Task 6 (final batch + verification).** Convert s3buckets (regional-client Delete closure). Run final checks: `grep -c "errgroup.WithContext" cmd/*.go` now returns only the out-of-scope four (rds/logs/images/elbv2); `grep -c "runOrphanPipeline" cmd/*.go` returns 11; LOC measurement vs the pre-B baseline. Commit any cleanup found.

Each task's per-file work follows the same recipe: identify the command's current `List`/`Process`/`Delete` sub-logic, extract into spec fields, delete the old errgroup/channels/collector/delete-loop boilerplate, run build/vet/test, manual `--help` smoke test for at least one command per batch.

**Per-batch verification** (same as refactor A):

- `go build ./... && go vet ./... && go test ./...` clean.
- `go run . <cmd> --help` prints expected flags for one representative command per batch.
- `grep "errgroup" cmd/<converted files>.go` returns nothing.
- Commit message: conventional-commits with scope `cmd` or `cmd/<file>`, type `refactor`, required body.

## Risks

- **Per-item processor latency.** Commands with heavy CloudWatch enrichment (lambda/dynamodb/elasticache/opensearch) currently parallelize across 10 workers. The helper preserves that exact worker count (`NumGoroutines=10`), so per-item throughput should be identical.
- **Sort-by streaming interaction.** `StreamTable.SetSort` forces full buffering — behavior unchanged since the helper sets it from `flagValues` exactly like the current code does.
- **Delete error handling divergence.** The helper uses fail-fast (`return err` on first delete failure), which matches the majority of existing commands. A spot-check confirmed lambda's current delete loop already logs-and-returns; s3buckets uses log-and-continue inside the loop but the overall function still returns the last error. Net: fail-fast preserves or slightly tightens current behavior — acceptable.
- **Enis filtering rewrites.** Enis' `executeENIs` has nontrivial per-item filtering; moving it into `Process` is a mechanical block move, but worth a careful diff during conversion. Plan step will call this out specifically.
- **Generics ergonomics.** Go 1.26 generics produce slightly verbose error messages on type mismatches (`OrphanPipeline[ec2types.Volume, volumeResult]`). Acceptable trade for type safety; IDE support mitigates.

## Out-of-scope follow-ups

- Running the helper on rds/logs/images/elbv2 via more knobs. Revisit only if all four start converging on a related pattern post-"C" work.
- Extracting the `rootCtx := ctx` pattern into the helper only — a narrower "pipeline-less" variant — for the sequential/non-pipeline commands. Separate design.
- Typed flag struct migration (still deferred from refactor A).

## Definition of done

After Task 6:

- 11 commands (listed above) use `runOrphanPipeline`.
- `cmd/pipeline.go` + `cmd/pipeline_test.go` exist.
- `grep -c "errgroup.WithContext" cmd/*.go` returns exactly 4 matches (rds, logs, images, elbv2).
- `grep -c "runOrphanPipeline" cmd/*.go` returns exactly 11 matches.
- Full test suite + build + vet clean.
- LOC in `cmd/` reduced by ~400-500 (spec declarations offset some of the removed ceremony).
- No user-visible behavior change across the 11 commands.
