## Contributing

Thank you for your interest in contributing to COR! This guide covers the project
structure and the patterns every command follows.

### Prerequisites

- Go 1.26 or later (see the `go` directive in `go.mod`)
- An AWS sandbox/dev account for manual testing — **never production**
- Familiarity with AWS SDK for Go v2 and the Cobra CLI framework

### Build / test / lint

The `Makefile` covers the common workflows:

```bash
make build              # compile ./cor with ldflags-injected version
make test               # go test ./...
make test-race          # go test -race ./...
make cover              # coverage profile + summary
make lint               # golangci-lint run ./...
make vet                # go vet ./...
make fmt                # gofmt -s -w .
make run ARGS='volumes --region us-east-1'
make help               # list all targets
```

CI runs `go vet`, `go build`, `go test -race -cover`, and `golangci-lint`. The
`.pre-commit-config.yaml` hooks run the same checks locally plus `commitlint`;
note that the Go hooks run against the **whole tree**, not just staged files, so
keep the working tree building and passing before you commit.

Dependencies are vendored into `vendor/`, which is **gitignored**. Run
`go mod tidy && go mod vendor` after changing `go.mod`.

### Repository layout

```
main.go                  entrypoint: signal context, optional pprof, cmd.Execute
cmd/
  root.go                rootCmd, AWSCommand, SharedSink, persistent flags, Viper init
  runner.go              runResourceCommand + runAcrossRegions — the command "shell"
  pipeline.go            OrphanPipeline + runOrphanPipeline — the concurrency "engine"
  helpers.go             tag/name/glob filters, confirmDelete, caches, CSV helpers
  idle.go                IsIdle / IdleSpec — shared CloudWatch idle probe
  regions.go             enabledRegions
  baseline.go            --save-baseline / --diff-baseline (NDJSON snapshots)
  cost.go, cost_rollups.go, pricing.go    cost rollup + pricing-cache commands
  <resource>.go          one file per AWS resource type
pkg/
  cost/                  USD type, rate table, Pricing accessors, CE actuals, cache, live refresh
  handlers/aws/          AWSClientImpl, NewConfig, custom retryer
  handlers/aws/awstest/  middleware-based fake AWS clients for tests
  handlers/flags/        GlobalFlags + GetFlags precedence resolution
  handlers/logging/      slog wrapper
  handlers/printer/      RowSink: streaming table, JSON, CSV
  handlers/prompter/     interactive yes/no prompt
  utils/                 generic slice helpers, EC2 instance-existence cache
```

### Architecture

Three layers. Understanding the split is most of what you need:

1. **The shell** — `runResourceCommand` (`cmd/runner.go`) owns flag resolution,
   AWS config, client construction, `AWSCommand` packaging, Cost Explorer gating,
   and single-vs-multi-region dispatch.
2. **The engine** — `runOrphanPipeline` (`cmd/pipeline.go`) owns the
   producer → workers → collector concurrency pattern, output streaming, cost
   decoration, baselines, run metrics, and the delete phase.
3. **The command** — `cmd/<resource>.go` supplies only the resource-specific
   parts: how to list it, whether it's orphaned, what to print, what it costs,
   and how to delete it.

**Do not hand-roll the errgroup pattern in a new command.** Use the pipeline.
Nested errgroups are fine *inside* a `PreScan`/`Process`/`Delete` callback when a
single item needs its own fan-out (see `cmd/snapshots.go`, `cmd/s3buckets.go`).

### Canonical command shape

`cmd/volumes.go` is the reference implementation. Every command looks like this:

```go
type orphanVolume struct {
    name, id, snapshotID, volumeType string
    size                             int32
}

var volumesCmd = &cobra.Command{
    Use:   "volumes",
    Short: "List and optionally delete unattached EBS volumes",
    RunE: func(cmd *cobra.Command, args []string) error {
        return runResourceCommand(cmd, CommandSetup{
            AdditionalFlags: []flags.Flag{
                {Name: "filter-by-name", Type: "string"},
            },
            BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
                return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
            },
        }, (*AWSCommand).executeVolumes)
    },
}

func (a *AWSCommand) executeVolumes(ctx context.Context, globals *flags.GlobalFlags, extras *map[string]any) error {
    filterByName := normalizeFilterValue(getFlagString(extras, "filter-by-name"))

    return runOrphanPipeline(a, ctx, globals, extras, OrphanPipeline[types.Volume, orphanVolume]{
        Headers:       []string{"Name", "Volume ID", "Snapshot ID", "Size"},
        ResourceLabel: "EBS volumes",
        List: func(ctx context.Context, emit func(types.Volume) error) error {
            // paginate; call emit once per item and propagate its error
        },
        Process: func(ctx context.Context, vol types.Volume) (*orphanVolume, error) {
            // enrich + filter; return (nil, nil) to skip this item
        },
        ToRow: func(r orphanVolume) []any {
            return []any{r.name, r.id, r.snapshotID, r.size}
        },
        MonthlyCost: func(r orphanVolume) cost.USD {
            return cost.USD(float64(r.size)) * a.Pricing.EBSVolumeGB(r.volumeType)
        },
        Delete: func(ctx context.Context, r orphanVolume) error {
            _, err := a.AWSClient.EC2.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(r.id)})
            return err
        },
        DeleteConcurrency: 5,
        DedupKey:          func(r orphanVolume) string { return r.id },
    })
}

func init() {
    volumesCmd.Flags().String("filter-by-name", "", "Filter volumes by tag:Name (empty = no filter).")
}
```

Notes on the shape:

- `RunE` is a thin call into `runResourceCommand`. The execute method is bound
  with Go's **method-expression** form, `(*AWSCommand).executeVolumes`, which
  passes the receiver as the first argument — don't change that argument order.
- `BuildClients` populates **only** the service clients the command uses.
  `AWSClientImpl` is a struct of pointers; unused fields stay nil.
- `runOrphanPipeline` is a top-level generic function, not a method, because Go
  does not allow type-parameterized methods.

### OrphanPipeline reference

| Field | Purpose |
|---|---|
| `Headers`, `ResourceLabel`, `HideIndex` | Column titles, the "Found N …" label, and whether to show the `#` column |
| `PreScan` | Runs once before producers start. Use for cross-resource maps (e.g. which snapshots an AMI references) |
| `List` / `Lists` | Producer(s). Set exactly one; `Lists` merges several producers into one stream |
| `Process` | Per-item enrichment and filtering. `(nil, nil)` skips the item; `(nil, err)` aborts the run |
| `ToRow` | Renders one result as row cells; length must match `Headers` |
| `Finalize` | Optional footer row. **Overrides** the automatic cost-total footer |
| `Delete` / `DeleteBatch` | Per-item delete, or one bulk call for APIs with native batch delete |
| `DeleteConcurrency` | Parallel deletes; defaults to 1 (sequential) when unset |
| `DeleteErrorPolicy` | Stop on first error (default) or continue; `--on-error continue` sets this |
| `DedupKey` | Stable per-result identity. **Required** for `--state-file` and `--diff-baseline` |
| `MonthlyCost` | Estimated monthly USD. Adds the `Est $/mo` column, totals footer, and cost filters |

### Invariants you must preserve

- **`rootCtx` for deletes.** Deletes run after `g.Wait()`. If any worker errored,
  the errgroup context is already cancelled, so reusing it would fail every
  delete with `context canceled`. The pipeline captures `rootCtx := ctx` up front
  and hands that to `Delete`. Never pass the errgroup context to a delete call.
- **Collect-then-confirm-once.** Commands never prompt per resource. The full
  table streams first, then `confirmDelete` asks once. Do not reintroduce
  per-item prompts.
- **Safe SDK accessors.** Always `aws.ToString(p)`, `aws.ToInt32(p)`,
  `aws.ToBool(p)`. A raw `*p` panics on nil, and most SDK response fields are
  pointers.
- **Paginators only.** Use `New<Op>Paginator`; never hand-roll `NextToken` loops.
  The few APIs that aren't paginated (e.g. `DescribeAddresses`) are single calls.
- **Flags go through `flags.GetFlags`.** Root flags arrive in the typed
  `*flags.GlobalFlags`; per-command flags arrive in the `extras` map. Precedence
  is CLI > config file > `COR_*` env > default, and it's already implemented —
  don't read `viper` or `cmd.Flags()` directly from a command.
- **Delete guards.** If an `--include-*` flag can surface a non-orphan row,
  re-assert the orphan condition inside `Delete` and skip. Several commands do
  this deliberately as a safety net.

### Cost model

Set `MonthlyCost` when the resource actually costs money; look the rate up
through `a.Pricing` (`pkg/cost`) rather than hardcoding a number. Add new SKUs to
`pkg/cost/rates.go` and expose them with an accessor in `pkg/cost/pricing.go`.

Leave `MonthlyCost` **nil** for free resources (IAM entities, ECS clusters, ASGs,
target groups). Those commands render no cost column; prefer a domain-specific
column such as `Last Used` or `Reason` instead. Unknown rates return 0, which
renders as `—`, deliberately signalling "not computed" rather than "$0.00".

### Account-global commands

If the resource's API is account-global (IAM, S3, Route53), add the command name
to the `globalServices` map in `cmd/runner.go`. Otherwise `--all-regions` will run
it once per region and emit N× duplicate rows.

### Adding a new resource type

1. Decide the **orphan criteria** — unreferenced, empty, detached, idle for N
   days, or in a specific state. Prefer server-side filters where the AWS API
   supports them, and put everything else in `Process`.
2. Create `cmd/<resource>.go` following the canonical shape above.
3. Register the command in `addSubcommandsPallets()` in `cmd/root.go`.
4. Add it to `globalServices` in `cmd/runner.go` if the API is account-global.
5. Add `MonthlyCost` (plus any new rate in `pkg/cost`) if the resource costs money.
6. Add `DedupKey` so `--state-file` and `--diff-baseline` work.
7. Write tests in `cmd/<resource>_test.go` using `awstest` (see below).
8. Update `README.md` and `cor.yaml`. Remember `cor.yaml` is a **flat** key space
   shared across commands — don't redeclare a key another command already defines.
9. Manually verify against a sandbox account: list first, then `--delete --dry-run`,
   and only then a real delete.

For resources that are idle-detected rather than detached, use the shared
`IsIdle`/`IdleSpec` helper in `cmd/idle.go` instead of writing new CloudWatch
plumbing. When a zero metric reading could just mean "too new to have data",
guard on the resource's age first — `cmd/vpcendpoints.go` and `cmd/bedrock.go`
show the pattern.

### Testing

Tests never touch real AWS. `pkg/handlers/aws/awstest` builds an `aws.Config`
whose middleware serves canned responses from a `Stubs` map and records calls;
any operation you didn't stub fails loudly rather than escaping to the network.

Command tests drive the real `rootCmd` and override the two seams in
`cmd/runner.go` — `newConfigFn` and `buildClientsFn`:

```go
func TestExecuteVolumes_FiltersAvailableOnly(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)
    prevCfg, prevBuild := newConfigFn, buildClientsFn
    t.Cleanup(func() { restoreRunnerSeams(prevCfg, prevBuild) })

    calls := &awstest.Calls{}
    fakeCfg := awstest.Config(t, calls, awstest.Stubs{
        "DescribeVolumes": &ec2.DescribeVolumesOutput{ /* ... */ },
    })
    newConfigFn = func(context.Context, handlers.CloudConfig, string, bool, bool) (*aws.Config, error) {
        return &fakeCfg, nil
    }
    buildClientsFn = func(CommandSetup, *aws.Config) *handlers.AWSClientImpl {
        return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(fakeCfg)}
    }

    out := &bytes.Buffer{}
    cmd := rootCmdForTest(out, []string{"volumes", "--region", "us-east-1"})
    if err := cmd.Execute(); err != nil {
        t.Fatalf("execute error: %v", err)
    }
    // assert on out.String() and calls.Count("DescribeVolumes")
}
```

Useful techniques:

- A stub value can be a function, `func(context.Context, any) (any, error)`, when
  the response must depend on the request (e.g. a different answer per role name).
- `calls.Count(op)` asserts how many times an operation ran; `calls.Names()`
  returns them in order, which is how delete-ordering tests work.
- Pass `--delete --yes` to exercise the delete phase without needing stdin.
- Assert on distinctive fixture names. Substring matching against table output
  will happily match `"tagged"` inside `"untagged"`.

Worth covering for a new command: the orphan predicate (including the negative
case that must *not* surface), each `--include-*` flag, the delete call sequence,
and that a per-item enrichment failure skips that item instead of aborting the run.

### Commit conventions

Conventional Commits, enforced by `commitlint`:

- Type is lower-case and one of
  `feat | fix | doc | style | refactor | test | chore | perf | build | ci | revert`
- **Scope is required and non-empty**, e.g. `feat(cmd/lambda): …`
- **Body is required**, separated from the header by a blank line
- Header ≤ 120 chars; body and footer lines ≤ 100 chars
- Subject must not end with `.` and must not be sentence/start/pascal/upper-case

### Security

- Never commit AWS credentials or secrets.
- Destructive operations must go through the pipeline's confirm-once flow, and
  must honour `--dry-run`.
- Exclude AWS-owned entities from deletion (service-linked IAM roles,
  AWS-managed policies, requester-managed ENIs).
- Document the IAM permissions a new command needs in `README.md`, keeping
  read-only and delete permissions listed separately.
- Prefer least privilege in examples.

### License

By contributing, you agree that your contributions will be licensed under the
Apache License 2.0.
