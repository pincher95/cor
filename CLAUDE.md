# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

COR (Cloud Orphan Resources) is a Go CLI that discovers — and optionally deletes — orphaned AWS resources across ~25 services (EBS, AMIs, ELB v1/v2, ASG, NAT, RDS, Lambda, S3, etc.). Built on Cobra + Viper + AWS SDK Go v2. Go **1.26.2**.

## Build / Test / Run

```bash
go build -o cor .                             # build (binary is gitignored)
go test ./...                                 # all tests
go test -cover ./...                          # with coverage
go test ./pkg/handlers/flags/...              # single package
go test -run TestGetFlags ./pkg/handlers/flags  # single test

./cor volumes --region us-east-1 --profile default
./cor <cmd> --delete                          # lists, prompts once, then deletes
COR_PPROF=1 COR_PPROF_ADDR=:6060 ./cor ...    # enable fgprof/pprof (off by default)
```

Dependencies are vendored locally into `vendor/` but the directory is **gitignored** — run `go mod vendor` after changing `go.mod` if you need offline builds.

Pre-commit (`.pre-commit-config.yaml`) runs `go-fmt`, `go-vet`, `go-imports`, `golangci-lint`, `go-unit-tests`, `go-build`, `go-mod-tidy`, plus `commitlint` on commit messages.

## Commit messages

Conventional-commits enforced via `@commitlint/config-conventional` (see `.opencommit-commitlint`):

- type ∈ `feat | fix | doc | style | refactor | test | chore | perf | build | ci | revert` (lower-case)
- **scope is required** and non-empty, e.g. `feat(cmd/lambda): ...`
- **body is required**, separated from header by a blank line
- header ≤ 120 chars; body/footer lines ≤ 100 chars; subject must not end with `.`

## Architecture

### Layer split

- **`cmd/<resource>.go`** — one file per AWS resource type. Each defines a `cobra.Command` var (e.g. `volumesCmd`), its `RunE`, its `init()` registering resource-specific flags, and an `execute<Resource>` method on `AWSCommand` with the actual logic. New commands are wired into `addSubcommandsPallets()` in `cmd/root.go`.
- **`cmd/root.go`** — defines `AWSCommand` (AWSClient + Logger + Prompter + Output), persistent root flags (`--region`, `--profile`, `--auth-method`, `--delete`, `--sort-by`, `--sort-desc`, `--timeout`, `--config`), Viper init, and timeout context plumbing via `PersistentPreRunE`/`PersistentPostRun`.
- **`cmd/helpers.go`** — shared: `confirmDelete`, tag-filter parsing (`parseTagFilters` / `tagsMatchFilters`), glob name matching (`matchesFilterValue` accepts `*`/`?`), ELB-tag map/format helpers.
- **`pkg/handlers/aws/client.go`** — `AWSClientImpl` struct bundles all service clients (EC2, ELB, STS, RDS, Lambda, CW, CWL, ...); only populate the ones a command needs. `NewConfig` picks auth (`AWS_CREDENTIALS_FILE` vs `ENV_SECRET`) and installs a **custom retryer that fail-fasts on auth/signing errors** (`RequestExpired`, `ExpiredToken`, `SignatureDoesNotMatch`, ...) while keeping 20 attempts for throttling.
- **`pkg/handlers/flags`** — `CommandFlagRetriever` + `GetFlags` implement precedence **CLI > YAML config > `COR_*` env > defaults**. Flag lookup walks `Flags() → InheritedFlags() → PersistentFlags()` so persistent root flags resolve from any subcommand. Don't bypass this — always go through `flags.GetFlags` with an `additionalFlags []flags.Flag` list.
- **`pkg/handlers/printer`** — `StreamTable` writes rows in chunks of 200 as they arrive (bounded memory). `SetSort(col, desc)` forces full buffering (streaming off), so warn users that `--sort-by` is incompatible with very large result sets.
- **`pkg/utils/cache.go`** — `InstanceCache` de-duplicates EC2 instance existence lookups within a single command (used when many ELB targets reference the same instance).

### Canonical command shape

Every `cmd/*.go` follows this exact skeleton — match it when adding a resource:

1. `RunE` builds: `prompter`, `ctx := cmd.Context()`, `flags.GetFlags(flagRetriever, additionalFlags)`, `handlers.NewConfig(...)`, per-service clients attached to a minimal `&handlers.AWSClientImpl{...}`, then calls a `run<Resource>Cmd` helper.
2. `run<Resource>Cmd` wraps client/logger/prompter/output into an `AWSCommand` and delegates to `(*AWSCommand).execute<Resource>(ctx, globals, extras)` where `globals` is `*flags.GlobalFlags` and `extras` is the per-command flag map from `flags.GetFlags`.
3. `execute<Resource>` runs the producer→workers→collector pattern (below), then the delete phase.

See `cmd/volumes.go` and `cmd/autoscaling.go` as canonical references.

### Producer → workers → collector (errgroup)

Long list+filter commands use `errgroup.WithContext(ctx)` with three stages:

- **Producer** (one `g.Go`): paginate the AWS list API with `New<Op>Paginator`, push items onto a buffered channel (cap 10–50), close the channel on return.
- **Workers** (`NumGoroutines = 10`, defined in `cmd/root.go`): N × `g.Go` each read from the item channel, enrich per-item (extra `Describe*` calls, CloudWatch metrics), and push `table.Row` onto a results channel. Workers must `select` on `egCtx.Done()` for cancellation.
- **Collector** (plain `go func()`, not in the errgroup): reads from the results channel, calls `stream.WriteRow(...)`, accumulates delete candidate IDs.

After `g.Wait()`: close the results channel, wait for the collector to drain, then run the delete phase.

### The `rootCtx` pattern for deletes (critical)

Deletes run **after** `g.Wait()`. If any worker returned an error, the errgroup's context is cancelled — reusing it for `DeleteXxx` calls would instantly fail with `context canceled`. Every `execute<Resource>` that can delete captures `rootCtx := ctx` at the top of the function and passes `rootCtx` (not `egCtx`, not the outer `ctx` which may be shadowed) to delete API calls. This was the fix in commit `56c5bb0` — preserve it in new commands.

### Delete flow (collect-then-confirm-once)

Commands do **not** prompt per-resource. They accumulate candidate IDs during listing, then call `confirmDelete(Prompter, Logger)` once after the full table has been printed. User sees the complete candidate set before typing `yes`. This was refactored repo-wide away from per-row prompts — do not reintroduce per-item prompts.

### AWS SDK conventions

- Always use `aws.ToString(p)`, `aws.ToInt32(p)`, `aws.ToBool(p)`, etc. — raw `*p` panics on nil. This applies to every SDK response field.
- Use `New<Op>Paginator` helpers; never hand-roll `NextToken` loops.
- `AWSClientImpl` is a struct of pointers — zero values for unused services are fine, so only set the fields a command actually uses.

### Config / env precedence

CLI flag > `--config` YAML > `COR_*` env var > default. Env var names = flag name with `-`→`_` and `COR_` prefix (`--sort-by` → `COR_SORT_BY`). The config file path defaults to `$HOME/.cor.yaml`. See `cor.yaml` in the repo root for an annotated example, and `cmd/root.go:initConfig` + `pkg/handlers/flags/flags.go:GetFlags` for the resolution logic.

## Repo quirks

- `investigations/` is gitignored scratch work (shell scripts, TSVs, reports from past audits). Not source — ignore when searching.
- `remotestore_stats_all.json` at the repo root is a gitignored data file; ignore it.
- `pkg/utils/extentions.go` is spelled with the typo (extentions, not extensions) — do not rename in unrelated PRs.
- The binary name `cor` is gitignored at the repo root.
