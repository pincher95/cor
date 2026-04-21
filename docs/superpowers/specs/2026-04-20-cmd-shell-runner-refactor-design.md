# `cmd/` Shell Runner Refactor — Design

**Status:** Design approved 2026-04-20. Implementation plan to follow.

## Motivation

Every file in `cmd/` (~25 commands, 7,597 LOC) opens with ~50–60 lines of near-identical boilerplate: prompter construction, flag retrieval, `CloudConfig` assembly, `handlers.NewConfig` call, per-service client instantiation, packing everything into an `AWSCommand`, then delegating to an `executeX` method. The layer between `RunE` and `executeX` — a thin `run<Resource>Cmd` wrapper — exists in every file and is also near-identical.

This boilerplate:

- Adds a fixed ~1,500 LOC repetition tax before any real work.
- Drowns the *interesting* parts of a command (flag declarations, business logic) in noise.
- Makes adding a new command an exercise in copy-paste rather than composition, which amplifies the chance of drift (e.g. a command quietly skipping the `rootCtx := ctx` pattern, or forgetting to pass `--sort-by` into the streaming table).

Collapsing the outer shell into a shared runner is the highest-leverage low-risk change available: it shortens every existing file, makes the next command cheaper to add, and clarifies where the actual variation lives.

## Scope

### In scope

- Introduce a single shared entry point in a new file `cmd/runner.go` that handles the boilerplate phase (flags → config → clients → `AWSCommand` → execute).
- Convert all 25 command files in `cmd/` to use it.
- Delete all 25 `run<Resource>Cmd` wrapper functions.
- Add one focused test for the new runner.

### Out of scope (explicitly deferred)

- **Errgroup producer → workers → collector pipeline abstraction.** 15 of 25 commands use it; abstracting it well requires modeling real variation (multi-call enrichment, cross-item state, multi-row outputs). Tracked as a separate future design.
- **`flagValues *map[string]any` → typed struct.** Changing this would ripple into every `executeX` body — outside this refactor's "outer shell only" boundary.
- **`AWSClientImpl` restructure.** The god-struct stays; commands continue to populate only the service-client fields they need.
- **Logger / prompter / output injection for testing.** The runner hard-codes production wiring (`logging.NewLogger`, `prompter.NewConsolePrompter(os.Stdin, os.Stdout)`, `os.Stdout`); if test seams are needed later, they can be added incrementally.
- **Changes to `addSubcommandsPallets()` or flag registration.** Flags continue to be declared in each file's `init()` via `cobra.Command.Flags().X(...)`; `root.go` still registers all subcommands explicitly.

## Architecture

### New file: `cmd/runner.go`

```go
package cmd

import (
    "context"
    "io"
    "os"

    "github.com/aws/aws-sdk-go-v2/aws"
    handlers "github.com/pincher95/cor/pkg/handlers/aws"
    "github.com/pincher95/cor/pkg/handlers/flags"
    "github.com/pincher95/cor/pkg/handlers/logging"
    "github.com/pincher95/cor/pkg/handlers/prompter"
    "github.com/spf13/cobra"
)

// CommandSetup declares per-command variation for runResourceCommand.
// AdditionalFlags lists flags specific to this subcommand (global flags are
// always included). BuildClients constructs the minimal AWSClientImpl the
// command needs from the resolved aws.Config.
type CommandSetup struct {
    AdditionalFlags []flags.Flag
    BuildClients    func(cfg *aws.Config) *handlers.AWSClientImpl
}

// newConfigFn is a test seam: tests override it to avoid hitting real AWS config.
var newConfigFn = handlers.NewConfig

func runResourceCommand(
    cmd *cobra.Command,
    setup CommandSetup,
    execute func(awsCmd *AWSCommand, ctx context.Context, flagValues *map[string]any) error,
) error {
    ctx := cmd.Context()

    flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
    flagValues, err := flags.GetFlags(flagRetriever, setup.AdditionalFlags)
    if err != nil {
        return err
    }

    cloudConfig := handlers.CloudConfig{
        AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
        Profile:    aws.String((*flagValues)["profile"].(string)),
        Region:     aws.String((*flagValues)["region"].(string)),
    }
    cfg, err := newConfigFn(ctx, cloudConfig, "UTC", true, true)
    if err != nil {
        return err
    }

    awsCmd := newAWSCommand(setup.BuildClients(cfg), os.Stdin, os.Stdout)
    return execute(awsCmd, ctx, flagValues)
}

// newAWSCommand assembles an AWSCommand with the default logger/prompter/output
// wiring. Split out for readability and to make the runner easier to test.
func newAWSCommand(client *handlers.AWSClientImpl, in io.Reader, out io.Writer) *AWSCommand {
    return &AWSCommand{
        AWSClient: *client,
        Logger:    logging.NewLogger(),
        Prompter:  prompter.NewConsolePrompter(in, out),
        Output:    out,
    }
}
```

Key notes:

- The `execute` parameter uses Go's **method-expression** form: passing `(*AWSCommand).executeVolumes` at the call site binds the `*AWSCommand` as the first argument, matching the function type. Zero changes required to the existing `executeX` methods.
- `newConfigFn` is declared as a package-level var rather than a struct field so the override is transparent to callers; tests swap it in a deferred cleanup.

### Per-command shape (after refactor)

```go
var volumesCmd = &cobra.Command{
    Use:   "volumes",
    Short: "List and optionally delete unattached EBS volumes",
    Long:  `List EBS volumes in 'available' state ...`,
    RunE: func(cmd *cobra.Command, args []string) error {
        return runResourceCommand(cmd, CommandSetup{
            AdditionalFlags: []flags.Flag{{Name: "filter-by-name", Type: "string"}},
            BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
                return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
            },
        }, (*AWSCommand).executeVolumes)
    },
}

func init() {
    volumesCmd.Flags().String("filter-by-name", "", "Filter volumes by tag:Name (empty = no filter).")
}

// executeVolumes and its helpers: untouched.
```

Every command converges to this exact shape — only the `Use`/`Short`/`Long` strings, the `AdditionalFlags` slice, the `BuildClients` body, and the bound method-expression differ.

### Deletions

- All 25 `run<Resource>Cmd(ctx, prompter, output, awsClient, flagValues)` wrapper functions.
- The inline `CloudConfig{}`, `handlers.NewConfig(...)`, prompter construction, logger construction, and `AWSCommand{}` literal in every `RunE`.

### What stays identical

- All `executeX` method bodies (producer → workers → collector pattern, `rootCtx := ctx` delete pattern, collect-then-confirm-once flow).
- `pkg/handlers/aws/client.go`, including `AWSClientImpl` as a god-struct.
- Flag declarations in each file's `init()`.
- `cmd/root.go` — persistent flag registration, Viper init, `addSubcommandsPallets()`.
- All existing tests (`flags_test.go`, `stream_test.go`, `extentions_test.go`).

## Testing

Add `cmd/runner_test.go` with one happy-path test:

- Override `newConfigFn` with a stub returning a zero-value `*aws.Config` and nil error.
- Call `runResourceCommand` with a fake `CommandSetup` whose `BuildClients` returns a minimal `AWSClientImpl{}`.
- Pass an `execute` callback that captures the received `*AWSCommand`, `ctx`, and `flagValues` into closure-captured variables.
- Assert: `BuildClients` was invoked once with the stubbed config; `execute` was invoked; the captured `flagValues` contains the expected global keys (`region`, `profile`, `auth-method`, `delete`, `sort-by`, `sort-desc`) plus the `AdditionalFlags` keys declared in the test.

Deliberately light: the runner is mechanical plumbing; deeper coverage belongs in `flags_test.go` (already present) and in per-command tests (future work, out of scope here).

## Migration strategy

Single PR, single commit. The changes are mechanical:

1. Add `cmd/runner.go`.
2. For each of the 25 command files:
   a. Replace the `RunE` body with a `runResourceCommand` call.
   b. Delete the `run<Resource>Cmd` wrapper function.
   c. Remove now-unused imports (usually `io`, `os`, `logging`, `prompter`, sometimes others).
3. Add `cmd/runner_test.go`.
4. Verify `go build ./...`, `go vet ./...`, `go test ./...` all pass.
5. Smoke-test: `./cor --help`, `./cor volumes --help`, `./cor <cmd> --region <r> --profile <p>` for 2–3 representative commands (one pipeline-style like `volumes`, one sequential like `autoscaling`, one with richer flags like `lambda`).

The per-file diffs are nearly identical; a reviewer spot-checks 2–3 and trusts the rest. The typecheck plus `--help` smoke test catches any signature drift or missing flag registration.

## Risks

- **Method-expression compatibility.** Requires every `executeX` to already have the exact signature `func (v *AWSCommand) executeX(ctx context.Context, flagValues *map[string]any) error`. Verified during exploration — all 25 commands match this pattern today. Any divergence found during migration is a signal to normalize the command, not to loosen the runner's type.
- **Unused-import churn.** ~20 commands will lose `os`, `io`, `logging`, `prompter` imports because those are now encapsulated. Handled file-by-file; `goimports` (part of pre-commit) cleans up automatically.
- **Hidden per-command deviations.** A few commands may pass non-default arguments to `handlers.NewConfig` (timezone, humanize, debug flags). Audit during implementation — all current call sites pass `"UTC", true, true`, so the runner's hard-coded values match. If a deviation is found, the plan surfaces it as a discrete item rather than silently normalizing behavior.
- **Test seam via package-level var.** `newConfigFn` is a global override point; tests must defer-restore it to avoid cross-test pollution. The runner test is the only consumer today, so the risk is small.

## Out-of-scope follow-ups

This refactor is the "A" step in a pre-agreed three-part sequence:

- **A (this spec).** Outer shell runner.
- **B (future).** Errgroup producer → workers → collector pipeline abstraction — design once A is merged and the real shape of each `executeX` is visible.
- **C (future).** New orphan-resource commands and cross-cutting features (JSON/CSV output, multi-region fan-out, cost columns). Both become cheaper on top of A and B.
