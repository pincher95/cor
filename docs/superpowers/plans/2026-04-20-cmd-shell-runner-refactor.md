# `cmd/` Shell Runner Refactor — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse the ~50–60 lines of near-identical RunE/config/client-construction boilerplate in each `cmd/*.go` file (25 files, ~1,500 LOC of repetition) into a single shared runner helper, without touching any `executeX` business logic.

**Architecture:** Add one new file `cmd/runner.go` exporting `runResourceCommand(cmd, CommandSetup, execute)` which performs prompter creation, flag retrieval, AWS config resolution, client assembly, and `AWSCommand` packaging. Each command's `RunE` becomes a single call into the runner with a `CommandSetup` literal declaring that command's `AdditionalFlags` and `BuildClients` callback, bound to the existing `executeX` method via Go's method-expression form. All 25 `run<Resource>Cmd` wrapper functions get deleted. Spec: `docs/superpowers/specs/2026-04-20-cmd-shell-runner-refactor-design.md`.

**Tech Stack:** Go 1.26.2, Cobra, Viper, AWS SDK Go v2, testing via stdlib `testing` package.

---

## Prerequisites

Read `CLAUDE.md` at repo root for:
- Commit-message convention (`commitlint` requires type + scope + blank line + body; header ≤120 chars, body lines ≤100 chars).
- Pre-commit hooks (`go-fmt`, `go-vet`, `go-imports`, `golangci-lint`, `go-unit-tests`, `go-build`, `go-mod-tidy`).
- The `rootCtx := ctx` pattern (we do NOT touch it — it lives inside `executeX`).

The pre-existing `go.mod`/`go.sum` modifications in the working tree are unrelated to this work; leave them alone (do not stage them).

### Non-canonical commands requiring prep first

Two commands deviate from the canonical `RunE → run<Name>Cmd → executeX` shape and must be normalized **before** the runner is introduced. They are handled in Tasks 1 and 2.

1. **`cmd/elbv1.go`** — entire command logic inline in `RunE`, no `run*Cmd` wrapper, no `executeElbv1` method. Uses `sync.WaitGroup` (not `errgroup`) and an ELB v1 client that is **not** in `AWSClientImpl`.
2. **`cmd/s3buckets.go`** — `executeS3Buckets(ctx, flagValues, cloudConfig)` takes a third parameter (for creating regional S3 clients). Does not match the method-expression signature the runner requires.

Five other commands (ecs, elasticache, lambda, dynamodb, opensearch) pass a `*logging.Logger` as an extra wrapper arg — harmless, gets dropped at conversion time because `a.Logger` is already populated inside `executeX`.

---

## Task 1: Prep — add ELBv1 client type and extract `cmd/elbv1.go` to canonical shape

**Why:** elbv1's logic is inline in its `RunE` and it uses a service client type not present in `AWSClientImpl`. Both must be fixed before the runner can be applied uniformly. This task makes no runtime-behavior change — it only moves code into the canonical location.

**Files:**
- Modify: `pkg/handlers/aws/client.go` — add `ELBv1 *elasticloadbalancing.Client` field to `AWSClientImpl`.
- Modify: `cmd/elbv1.go` — extract the inline RunE body into `executeElbv1` method + `runElbv1Cmd` wrapper.

### Steps

- [ ] **Step 1: Add ELBv1 field to AWSClientImpl**

Edit `pkg/handlers/aws/client.go`. Add the import:

```go
"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
```

Add the field inside the `AWSClientImpl` struct (near the other ELB field):

```go
type AWSClientImpl struct {
    EC2         *ec2.Client
    ELB         *elasticloadbalancingv2.Client
    ELBv1       *elasticloadbalancing.Client
    STS         *sts.Client
    // ... (rest unchanged)
}
```

- [ ] **Step 2: Verify build still works**

Run: `go build ./...`
Expected: success (new field is unused but valid).

- [ ] **Step 3: Rewrite `cmd/elbv1.go` RunE to match canonical shape**

The existing `elbv1Cmd.RunE` body (lines 43–181 in `cmd/elbv1.go`) does: logger+prompter setup, flag retrieval, AWS config, client creation, and ~120 lines of business logic (WaitGroup-based pipeline + deletion).

Replace the entire RunE block with the canonical shape. Keep `init()` as-is.

```go
var elbv1Cmd = &cobra.Command{
    Use:   "elbv1",
    Short: "Return ELB of type Classic",
    Long:  `Return Classic ELB with instance state unhealthy.`,
    RunE: func(cmd *cobra.Command, args []string) error {
        prompterClient := prompter.NewConsolePrompter(os.Stdin, os.Stdout)
        output := os.Stdout
        ctx := cmd.Context()

        flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
        additionalFlags := []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "filter-by-tags", Type: "string"},
            {Name: "show-unhealthy", Type: "bool"},
            {Name: "show-tags", Type: "bool"},
        }
        flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
        if err != nil {
            return err
        }

        cloudConfig := &handlers.CloudConfig{
            AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
            Profile:    aws.String((*flagValues)["profile"].(string)),
            Region:     aws.String((*flagValues)["region"].(string)),
        }
        cfg, err := handlers.NewConfig(ctx, *cloudConfig, "UTC", true, true)
        if err != nil {
            return err
        }

        awsClient := &handlers.AWSClientImpl{
            ELBv1: elasticloadbalancing.NewFromConfig(*cfg),
            EC2:   ec2.NewFromConfig(*cfg),
        }
        return runElbv1Cmd(ctx, prompterClient, output, awsClient, flagValues)
    },
}

func runElbv1Cmd(ctx context.Context, prompter prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any) error {
    command := &AWSCommand{
        AWSClient: *awsClient,
        Logger:    logging.NewLogger(),
        Prompter:  prompter,
        Output:    output,
    }
    return command.executeElbv1(ctx, flagValues)
}
```

- [ ] **Step 4: Add `executeElbv1` method with the extracted logic**

Append after `runElbv1Cmd` in `cmd/elbv1.go`. This is the logic that currently lives inline in `RunE`, with these substitutions:

- `logger` → `e.Logger`
- `prompterClient` → `e.Prompter`
- `os.Stdout` (in `printer.NewStreamTable`) → `e.Output`
- `client` (v1 ELB) → `e.AWSClient.ELBv1`
- `ec2Client` → `e.AWSClient.EC2`

```go
func (e *AWSCommand) executeElbv1(ctx context.Context, flagValues *map[string]any) error {
    collectDeletes := (*flagValues)["delete"].(bool)

    var wg sync.WaitGroup
    loadBalancerChan := make(chan types.LoadBalancerDescription, 100)
    tableRowChan := make(chan elbv1Result, 100)
    errorChan := make(chan error, 1)

    showUnhealthy := (*flagValues)["show-unhealthy"].(bool)
    showTags := (*flagValues)["show-tags"].(bool)
    filterByName := normalizeFilterValue((*flagValues)["filter-by-name"].(string))
    tagFilters := parseTagFilters((*flagValues)["filter-by-tags"].(string))

    headers := []string{"LoadBalancer Name", "number of listeners", "targets without instances"}
    if showUnhealthy {
        headers = append(headers, "targets unhealthy")
    }
    headers = append(headers, "VPC ID")
    if showTags {
        headers = append(headers, "Tags")
    }
    stream := printer.NewStreamTable(e.Output, true, headers)
    stream.SetSort((*flagValues)["sort-by"].(string), (*flagValues)["sort-desc"].(bool))
    defer stream.Close()

    wg.Go(func() {
        if err := describeLoadBalancers(ctx, e.AWSClient.ELBv1, loadBalancerChan); err != nil {
            errorChan <- err
            close(loadBalancerChan)
            return
        }
        close(loadBalancerChan)
    })

    for lb := range loadBalancerChan {
        wg.Go(func() {
            if !matchesFilterValue(aws.ToString(lb.LoadBalancerName), filterByName) {
                return
            }
            tableRow, deleteName, err := handleLoadBalancer(ctx, &lb, e.AWSClient.ELBv1, e.AWSClient.EC2, showUnhealthy, showTags, tagFilters)
            if err != nil {
                errorChan <- err
                return
            }
            if tableRow != nil {
                tableRowChan <- elbv1Result{row: tableRow, deleteName: deleteName}
            }
        })
    }

    doneChan := make(chan struct{})
    go func() {
        wg.Wait()
        close(doneChan)
    }()

    deleteNames := make([]string, 0)
    for {
        select {
        case err := <-errorChan:
            e.Logger.LogError("Error during loadbalancer processing", err, nil, true)
            return err
        case res := <-tableRowChan:
            if res.row != nil && len(*res.row) > 0 {
                stream.WriteRow((*res.row)...)
            }
            if collectDeletes && res.deleteName != "" {
                deleteNames = append(deleteNames, res.deleteName)
            }
        case <-doneChan:
            close(tableRowChan)
            for res := range tableRowChan {
                if res.row == nil || len(*res.row) == 0 {
                    continue
                }
                stream.WriteRow((*res.row)...)
                if collectDeletes && res.deleteName != "" {
                    deleteNames = append(deleteNames, res.deleteName)
                }
            }
            if !collectDeletes || len(deleteNames) == 0 {
                return nil
            }
            confirm, err := confirmDelete(e.Prompter, e.Logger)
            if err != nil {
                return err
            }
            if !confirm {
                return nil
            }
            for _, lbName := range deleteNames {
                e.Logger.LogInfo("Deleting LoadBalancer", map[string]any{"LoadBalancerName": lbName})
                if _, err := e.AWSClient.ELBv1.DeleteLoadBalancer(ctx, &elasticloadbalancing.DeleteLoadBalancerInput{
                    LoadBalancerName: aws.String(lbName),
                }); err != nil {
                    return err
                }
            }
            return nil
        }
    }
}
```

Imports on `cmd/elbv1.go` stay the same — the file still needs `sync`, `elasticloadbalancing`, etc.

- [ ] **Step 5: Run build and vet**

Run: `go build ./... && go vet ./...`
Expected: success, no warnings.

- [ ] **Step 6: Run tests**

Run: `go test ./...`
Expected: all existing tests pass (elbv1 has no tests today).

- [ ] **Step 7: Smoke-test elbv1 help output**

Run: `go run . elbv1 --help`
Expected: usage block prints with the four `--filter-by-name`, `--filter-by-tags`, `--show-unhealthy`, `--show-tags` flags plus the global persistent flags (`--region`, `--profile`, etc.).

- [ ] **Step 8: Commit**

```bash
git add pkg/handlers/aws/client.go cmd/elbv1.go
git commit -m "$(cat <<'EOF'
refactor(cmd/elbv1): extract inline RunE into executeElbv1 method

Moves the ~140 lines of business logic previously inlined in elbv1Cmd.RunE
into a standard executeElbv1 method on *AWSCommand plus a runElbv1Cmd
wrapper, matching the canonical shape used by every other command. Adds
ELBv1 *elasticloadbalancing.Client to AWSClientImpl so the extracted
method can reach the v1 client through the shared struct. Behavior is
unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Prep — add `CloudConfig` to `AWSCommand` and normalize `cmd/s3buckets.go`

**Why:** `executeS3Buckets` takes `cloudConfig` as a third parameter so its internal helpers can construct regional S3 clients on the fly. The runner cannot match that signature with a method expression. Move `CloudConfig` onto `AWSCommand` as a field so s3buckets can reach it from the receiver. No runtime-behavior change.

**Files:**
- Modify: `cmd/root.go` — add `CloudConfig *handlers.CloudConfig` field to `AWSCommand` struct.
- Modify: `cmd/s3buckets.go` — drop `cloudConfig` param from `executeS3Buckets`, `checkS3BucketOrphan`, `abortMultipartUploads`, `deleteS3Bucket`; read `a.CloudConfig` instead. Populate it in `runS3BucketsCmd`.

### Steps

- [ ] **Step 1: Add `CloudConfig` field to `AWSCommand`**

Edit `cmd/root.go`. Update the struct definition (around line 37):

```go
type AWSCommand struct {
    AWSClient   handlers.AWSClientImpl
    CloudConfig *handlers.CloudConfig
    Logger      *logging.Logger
    Prompter    prompter.Client
    Output      io.Writer
}
```

- [ ] **Step 2: Update `runS3BucketsCmd` to populate `CloudConfig`**

Edit `cmd/s3buckets.go`. Replace the existing `runS3BucketsCmd` function (around line 105):

```go
func runS3BucketsCmd(ctx context.Context, prompter *prompter.Client, output io.Writer, awsClient *handlers.AWSClientImpl, flagValues *map[string]any, logger *logging.Logger, cloudConfig *handlers.CloudConfig) error {
    command := &AWSCommand{
        AWSClient:   *awsClient,
        CloudConfig: cloudConfig,
        Logger:      logger,
        Prompter:    *prompter,
        Output:      output,
    }
    return command.executeS3Buckets(ctx, flagValues)
}
```

- [ ] **Step 3: Drop `cloudConfig` param from `executeS3Buckets`**

Edit the signature (around line 116) and update the two call sites inside the function that used to pass `cloudConfig` — one in the worker call to `checkS3BucketOrphan` (around line 158), one in the delete loop calls to `abortMultipartUploads` / `deleteS3Bucket` (around lines 228, 238):

```go
func (a *AWSCommand) executeS3Buckets(ctx context.Context, flagValues *map[string]any) error {
    // ... (body unchanged except for these lines:)

    // In the worker (was: a.checkS3BucketOrphan(egCtx, bucket, checkLifecycle, cloudConfig)):
    orphan, err := a.checkS3BucketOrphan(egCtx, bucket, checkLifecycle)

    // In the delete loop (was: a.abortMultipartUploads(rootCtx, bucket.BucketName, bucket.Region, cloudConfig)):
    if err := a.abortMultipartUploads(rootCtx, bucket.BucketName, bucket.Region); err != nil { ... }

    // Was: a.deleteS3Bucket(rootCtx, bucket.BucketName, bucket.Region, cloudConfig)
    if err := a.deleteS3Bucket(rootCtx, bucket.BucketName, bucket.Region); err != nil { ... }
}
```

- [ ] **Step 4: Drop `cloudConfig` param from the three helper methods**

Update the signatures of `checkS3BucketOrphan`, `abortMultipartUploads`, `deleteS3Bucket` and replace every `cloudConfig.X` reference with `a.CloudConfig.X`:

```go
func (a *AWSCommand) checkS3BucketOrphan(ctx context.Context, bucket s3types.Bucket, checkLifecycle bool) (*orphanS3Bucket, error) {
    // ... body unchanged, except:
    regionalConfig := &handlers.CloudConfig{
        AuthMethod: a.CloudConfig.AuthMethod,
        Profile:    a.CloudConfig.Profile,
        Region:     aws.String(region),
    }
    // ... rest unchanged
}

func (a *AWSCommand) abortMultipartUploads(ctx context.Context, bucketName string, region string) error {
    // ... body unchanged, except:
    regionalConfig := &handlers.CloudConfig{
        AuthMethod: a.CloudConfig.AuthMethod,
        Profile:    a.CloudConfig.Profile,
        Region:     aws.String(region),
    }
    // ... rest unchanged
}

func (a *AWSCommand) deleteS3Bucket(ctx context.Context, bucketName string, region string) error {
    // ... body unchanged, except:
    regionalConfig := &handlers.CloudConfig{
        AuthMethod: a.CloudConfig.AuthMethod,
        Profile:    a.CloudConfig.Profile,
        Region:     aws.String(region),
    }
    // ... rest unchanged
}
```

- [ ] **Step 5: Run build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 6: Smoke-test s3buckets help output**

Run: `go run . s3buckets --help`
Expected: usage block prints with the `--check-lifecycle` flag plus global persistent flags.

- [ ] **Step 7: Commit**

```bash
git add cmd/root.go cmd/s3buckets.go
git commit -m "$(cat <<'EOF'
refactor(cmd/s3buckets): move CloudConfig onto AWSCommand

executeS3Buckets used to take *handlers.CloudConfig as a third parameter
so its regional-S3-client helpers could re-resolve auth per bucket
region. Add CloudConfig *handlers.CloudConfig to AWSCommand and have the
three helpers read it off the receiver, so the execute method matches
the canonical (ctx, flagValues) signature the upcoming shared runner
will bind to via a method expression. No behavior change.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Add `cmd/runner.go` and its test (TDD)

**Files:**
- Create: `cmd/runner_test.go`
- Create: `cmd/runner.go`

### Steps

- [ ] **Step 1: Write the failing test**

Create `cmd/runner_test.go` with exactly this content:

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
    "testing"

    "github.com/aws/aws-sdk-go-v2/aws"
    handlers "github.com/pincher95/cor/pkg/handlers/aws"
    "github.com/pincher95/cor/pkg/handlers/flags"
    "github.com/spf13/cobra"
    "github.com/spf13/viper"
)

func TestRunResourceCommand_HappyPath(t *testing.T) {
    viper.Reset()
    t.Cleanup(viper.Reset)

    originalNewConfigFn := newConfigFn
    t.Cleanup(func() { newConfigFn = originalNewConfigFn })

    var newConfigCalled bool
    newConfigFn = func(ctx context.Context, cc handlers.CloudConfig, tz string, humanize, debug bool) (*aws.Config, error) {
        newConfigCalled = true
        if aws.ToString(cc.Region) != "us-west-2" {
            t.Errorf("expected region us-west-2, got %q", aws.ToString(cc.Region))
        }
        if aws.ToString(cc.Profile) != "default" {
            t.Errorf("expected profile default, got %q", aws.ToString(cc.Profile))
        }
        return &aws.Config{Region: aws.ToString(cc.Region)}, nil
    }

    cmd := &cobra.Command{Use: "test"}
    cmd.PersistentFlags().String("region", "us-west-2", "")
    cmd.PersistentFlags().String("profile", "default", "")
    cmd.PersistentFlags().String("auth-method", "AWS_CREDENTIALS_FILE", "")
    cmd.PersistentFlags().Bool("delete", false, "")
    cmd.PersistentFlags().String("sort-by", "", "")
    cmd.PersistentFlags().Bool("sort-desc", false, "")
    cmd.Flags().String("filter-by-name", "", "")
    cmd.SetContext(context.Background())

    var (
        buildClientsCalled bool
        executeCalled      bool
        capturedAWSCmd     *AWSCommand
        capturedFlagValues *map[string]any
        capturedCfg        *aws.Config
    )

    setup := CommandSetup{
        AdditionalFlags: []flags.Flag{{Name: "filter-by-name", Type: "string"}},
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            buildClientsCalled = true
            capturedCfg = cfg
            return &handlers.AWSClientImpl{}
        },
    }

    execute := func(awsCmd *AWSCommand, ctx context.Context, fv *map[string]any) error {
        executeCalled = true
        capturedAWSCmd = awsCmd
        capturedFlagValues = fv
        return nil
    }

    if err := runResourceCommand(cmd, setup, execute); err != nil {
        t.Fatalf("runResourceCommand returned error: %v", err)
    }

    if !newConfigCalled {
        t.Error("expected newConfigFn to be called")
    }
    if !buildClientsCalled {
        t.Error("expected BuildClients to be called")
    }
    if capturedCfg == nil {
        t.Error("expected BuildClients to receive non-nil config")
    }
    if !executeCalled {
        t.Fatal("expected execute to be called")
    }
    if capturedAWSCmd == nil {
        t.Fatal("expected execute to receive non-nil AWSCommand")
    }
    if capturedAWSCmd.Logger == nil {
        t.Error("expected AWSCommand.Logger to be set")
    }
    if capturedAWSCmd.Prompter == nil {
        t.Error("expected AWSCommand.Prompter to be set")
    }
    if capturedAWSCmd.Output == nil {
        t.Error("expected AWSCommand.Output to be set")
    }
    if capturedAWSCmd.CloudConfig == nil {
        t.Error("expected AWSCommand.CloudConfig to be set")
    }

    expectedKeys := []string{"region", "profile", "auth-method", "delete", "sort-by", "sort-desc", "filter-by-name"}
    for _, k := range expectedKeys {
        if _, ok := (*capturedFlagValues)[k]; !ok {
            t.Errorf("expected flagValue %q to be present", k)
        }
    }
    if (*capturedFlagValues)["region"].(string) != "us-west-2" {
        t.Errorf("expected region flag to be us-west-2, got %v", (*capturedFlagValues)["region"])
    }
}
```

- [ ] **Step 2: Run the test to confirm it fails**

Run: `go test ./cmd/ -run TestRunResourceCommand_HappyPath`
Expected: **compile error** mentioning `undefined: runResourceCommand`, `undefined: CommandSetup`, `undefined: newConfigFn`.

- [ ] **Step 3: Implement `cmd/runner.go`**

Create `cmd/runner.go` with exactly this content:

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

// newConfigFn is a test seam: tests override it to avoid hitting real AWS config resolution.
var newConfigFn = handlers.NewConfig

// runResourceCommand handles the boilerplate phase of every resource command:
// flag retrieval, AWS config resolution, client assembly, and AWSCommand packaging.
// The execute callback receives the fully assembled AWSCommand plus the flag values
// and performs the command-specific work. Bind existing methods via Go's
// method-expression form, e.g. (*AWSCommand).executeVolumes.
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

    cloudConfig := &handlers.CloudConfig{
        AuthMethod: aws.String((*flagValues)["auth-method"].(string)),
        Profile:    aws.String((*flagValues)["profile"].(string)),
        Region:     aws.String((*flagValues)["region"].(string)),
    }
    cfg, err := newConfigFn(ctx, *cloudConfig, "UTC", true, true)
    if err != nil {
        return err
    }

    awsCmd := newAWSCommand(setup.BuildClients(cfg), cloudConfig, os.Stdin, os.Stdout)
    return execute(awsCmd, ctx, flagValues)
}

// newAWSCommand assembles an AWSCommand with the default logger/prompter/output
// wiring. Split out for readability and testability.
func newAWSCommand(client *handlers.AWSClientImpl, cloudConfig *handlers.CloudConfig, in io.Reader, out io.Writer) *AWSCommand {
    return &AWSCommand{
        AWSClient:   *client,
        CloudConfig: cloudConfig,
        Logger:      logging.NewLogger(),
        Prompter:    prompter.NewConsolePrompter(in, out),
        Output:      out,
    }
}
```

- [ ] **Step 4: Run the test again to confirm it passes**

Run: `go test ./cmd/ -run TestRunResourceCommand_HappyPath -v`
Expected: `--- PASS: TestRunResourceCommand_HappyPath`.

- [ ] **Step 5: Run full build, vet, test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success across the board.

- [ ] **Step 6: Commit**

```bash
git add cmd/runner.go cmd/runner_test.go
git commit -m "$(cat <<'EOF'
refactor(cmd): add runResourceCommand shared helper

Introduces CommandSetup + runResourceCommand in cmd/runner.go to absorb
the flag-retrieval, AWS-config, client-assembly, and AWSCommand-packaging
boilerplate that currently lives inlined in every cmd/*.go RunE. The
execute callback uses Go's method-expression form so existing executeX
methods bind without signature changes. newConfigFn is a package-level
test seam; runner_test.go exercises the happy path via it.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Conversion recipe (applied identically in Tasks 4 through 10)

Every command conversion follows the same mechanical transformation. The pilot task (Task 4) applies it to `volumes.go`; Tasks 5–10 apply it in batches of four.

**Per-file transformation:**

1. **Replace** the entire `RunE` body with a single call to `runResourceCommand`, passing:
   - The same `AdditionalFlags` slice literal that was previously built inside RunE.
   - A `BuildClients` closure that constructs the same service clients and packages them into the same `&handlers.AWSClientImpl{...}` the old code did.
   - The bound method expression `(*AWSCommand).<executeMethod>`.
2. **Delete** the `run<Name>Cmd` wrapper function (it is no longer referenced).
3. **Clean up imports** — `goimports` (part of pre-commit) removes unused ones, but the subagent should run `goimports -w <file>` manually to catch them before commit. Imports typically removed: `"context"`, `"io"`, `"os"`, `"github.com/pincher95/cor/pkg/handlers/logging"`, `"github.com/pincher95/cor/pkg/handlers/prompter"`. Keep imports still used inside `executeX` / helper functions.
4. **Leave** the `init()` flag-registration block alone.
5. **Leave** the `executeX` method and all helper functions alone.

**Per-batch verification:**

After editing all files in a batch:
- `go build ./...` — expect success.
- `go vet ./...` — expect no warnings.
- `go test ./...` — expect pass.
- Run `go run . <cmd> --help` for at least one command in the batch — expect usage block with the correct flags.

---

## Task 4: Pilot — convert `cmd/volumes.go`

**Why:** Run the canonical conversion once end-to-end on a representative pipeline-style command before scaling. This is the walking-skeleton check.

**Files:** Modify: `cmd/volumes.go`

### Steps

- [ ] **Step 1: Replace the `volumesCmd` RunE block**

Edit `cmd/volumes.go`. Replace the `RunE:` field (lines ~53–95) with:

```go
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
```

- [ ] **Step 2: Delete the `runVolumeCmd` wrapper function**

Remove the entire `runVolumeCmd` function (lines ~98–108 in the current file). Nothing else references it.

- [ ] **Step 3: Clean imports**

Run: `goimports -w cmd/volumes.go`

This should remove the now-unused `"io"`, `"os"`, `"github.com/pincher95/cor/pkg/handlers/logging"`, `"github.com/pincher95/cor/pkg/handlers/prompter"` imports. `"context"` may be kept because `executeVolumes` still takes one.

If `goimports` is not on PATH, install with `go install golang.org/x/tools/cmd/goimports@latest`, or manually delete the unused imports and run `go build ./...` to verify nothing remains unused.

- [ ] **Step 4: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 5: Smoke-test**

Run: `go run . volumes --help`
Expected: usage block prints with `--filter-by-name` plus global persistent flags. No errors.

- [ ] **Step 6: Commit**

```bash
git add cmd/volumes.go
git commit -m "$(cat <<'EOF'
refactor(cmd/volumes): convert to shared runResourceCommand helper

Collapses volumesCmd.RunE from ~45 lines to ~8 by delegating the
flag/config/client setup phase to runResourceCommand. Deletes the
runVolumeCmd wrapper. executeVolumes and its helpers are unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Convert batch 1 — autoscaling, clientvpn, dynamodb, ecr

**Files:** Modify: `cmd/autoscaling.go`, `cmd/clientvpn.go`, `cmd/dynamodb.go`, `cmd/ecr.go`

### Steps

- [ ] **Step 1: Convert `cmd/autoscaling.go`**

Replace `autoscalingCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "force", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{ASG: autoscaling.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeAutoscaling)
},
```

Delete the `runAutoscalingCmd` function. Run `goimports -w cmd/autoscaling.go`.

- [ ] **Step 2: Convert `cmd/clientvpn.go`**

Replace `clientVPNCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "include-active", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeClientVPN)
},
```

Delete the `runClientVPNCmd` function. Run `goimports -w cmd/clientvpn.go`.

- [ ] **Step 3: Convert `cmd/dynamodb.go`**

Replace `dynamodbCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "days-no-activity", Type: "int"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{
                DynamoDB:   dynamodb.NewFromConfig(*cfg),
                CloudWatch: cloudwatch.NewFromConfig(*cfg),
            }
        },
    }, (*AWSCommand).executeDynamoDB)
},
```

Delete the `runDynamoDBCmd` function. Run `goimports -w cmd/dynamodb.go`. (Note: the old RunE constructed a `logger` and passed it into the wrapper; this logger is now discarded at conversion — `executeDynamoDB` uses `a.Logger` which the runner populates.)

- [ ] **Step 4: Convert `cmd/ecr.go`**

Replace `ecrCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "untagged-only", Type: "bool"},
            {Name: "older-than-days", Type: "string"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{ECR: ecr.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeECR)
},
```

Delete the `runECRCmd` function. Run `goimports -w cmd/ecr.go`.

- [ ] **Step 5: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 6: Smoke-test one command**

Run: `go run . dynamodb --help`
Expected: usage block with `--days-no-activity` plus global flags.

- [ ] **Step 7: Commit**

```bash
git add cmd/autoscaling.go cmd/clientvpn.go cmd/dynamodb.go cmd/ecr.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert autoscaling/clientvpn/dynamodb/ecr to shared runner

Collapses each RunE to a single runResourceCommand call and deletes the
per-file runXCmd wrappers. executeX methods and their helpers are
unchanged. Redundant logger construction in the old dynamodb RunE is
dropped since AWSCommand.Logger is populated by the runner.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Convert batch 2 — ecs, efs, elasticache, elasticaddresses

**Files:** Modify: `cmd/ecs.go`, `cmd/efs.go`, `cmd/elasticache.go`, `cmd/elasticaddresses.go`

### Steps

- [ ] **Step 1: Convert `cmd/ecs.go`**

Replace `ecsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{},
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{ECS: ecs.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeECS)
},
```

Delete `runECSCmd`. Run `goimports -w cmd/ecs.go`.

- [ ] **Step 2: Convert `cmd/efs.go`**

Replace `efsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "include-attached", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EFS: efs.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeEFS)
},
```

Delete `runEFSCmd`. Run `goimports -w cmd/efs.go`.

- [ ] **Step 3: Convert `cmd/elasticache.go`**

Replace `elasticacheCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "hours-zero-connections", Type: "int"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{
                ElastiCache: elasticache.NewFromConfig(*cfg),
                CloudWatch:  cloudwatch.NewFromConfig(*cfg),
            }
        },
    }, (*AWSCommand).executeElastiCache)
},
```

Delete `runElastiCacheCmd`. Run `goimports -w cmd/elasticache.go`.

- [ ] **Step 4: Convert `cmd/elasticaddresses.go`**

Replace `elasticIPsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeElasticIPs)
},
```

Delete `runElasticIPsCmd`. Run `goimports -w cmd/elasticaddresses.go`.

- [ ] **Step 5: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 6: Smoke-test one command**

Run: `go run . elasticache --help`
Expected: usage block with `--hours-zero-connections`.

- [ ] **Step 7: Commit**

```bash
git add cmd/ecs.go cmd/efs.go cmd/elasticache.go cmd/elasticaddresses.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert ecs/efs/elasticache/elasticaddresses to shared runner

Collapses each RunE to a runResourceCommand call and deletes the
per-file wrapper functions. executeX methods unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Convert batch 3 — elbv1, elbv2, enis, images

**Files:** Modify: `cmd/elbv1.go`, `cmd/elbv2.go`, `cmd/enis.go`, `cmd/images.go`

### Steps

- [ ] **Step 1: Convert `cmd/elbv1.go`**

(Task 1 normalized elbv1 into the canonical shape — this step now applies the same conversion as every other command.)

Replace `elbv1Cmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "filter-by-tags", Type: "string"},
            {Name: "show-unhealthy", Type: "bool"},
            {Name: "show-tags", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{
                ELBv1: elasticloadbalancing.NewFromConfig(*cfg),
                EC2:   ec2.NewFromConfig(*cfg),
            }
        },
    }, (*AWSCommand).executeElbv1)
},
```

Delete `runElbv1Cmd`. Run `goimports -w cmd/elbv1.go`.

- [ ] **Step 2: Convert `cmd/elbv2.go`**

Replace `elbv2Cmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "filter-by-tags", Type: "string"},
            {Name: "show-unhealthy", Type: "bool"},
            {Name: "show-tags", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{
                ELB: elasticloadbalancingv2.NewFromConfig(*cfg),
                EC2: ec2.NewFromConfig(*cfg),
            }
        },
    }, (*AWSCommand).executeElbv2)
},
```

Delete `runElbv2Cmd`. Run `goimports -w cmd/elbv2.go`.

- [ ] **Step 3: Convert `cmd/enis.go`**

Replace `enisCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "filter-by-enis", Type: "string"},
            {Name: "filter-by-vpc", Type: "string"},
            {Name: "filter-by-subnet", Type: "string"},
            {Name: "filter-by-sg", Type: "string"},
            {Name: "filter-by-type", Type: "string"},
            {Name: "filter-by-desc", Type: "string"},
            {Name: "filter-by-ip", Type: "string"},
            {Name: "filter-by-id-or-name", Type: "string"},
            {Name: "filter-by-vpc-id", Type: "string"},
            {Name: "filter-by-subnet-id", Type: "string"},
            {Name: "filter-by-security-group-id", Type: "string"},
            {Name: "filter-by-interface-type", Type: "string"},
            {Name: "filter-by-description", Type: "string"},
            {Name: "filter-by-private-ip", Type: "string"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeENIs)
},
```

Delete `runENIsCmd`. Run `goimports -w cmd/enis.go`.

- [ ] **Step 4: Convert `cmd/images.go`**

Replace `imagesCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "creation-date-before", Type: "string"},
            {Name: "creation-date-after", Type: "string"},
            {Name: "include-used-by-instance", Type: "bool"},
            {Name: "include-used-by-launch-template", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeImages)
},
```

Delete `runImagesCmd`. Run `goimports -w cmd/images.go`.

- [ ] **Step 5: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 6: Smoke-test one command**

Run: `go run . enis --help`
Expected: usage block with 15 filter flags (7 primary + 7 backwards-compat aliases + `filter-by-name`).

- [ ] **Step 7: Commit**

```bash
git add cmd/elbv1.go cmd/elbv2.go cmd/enis.go cmd/images.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert elbv1/elbv2/enis/images to shared runner

Collapses each RunE to a runResourceCommand call and deletes the
per-file wrapper functions. executeX methods and their helpers are
unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Convert batch 4 — lambda, logs, natgateways, opensearch

**Files:** Modify: `cmd/lambda.go`, `cmd/logs.go`, `cmd/natgateways.go`, `cmd/opensearch.go`

### Steps

- [ ] **Step 1: Convert `cmd/lambda.go`**

Replace `lambdaCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "days-since-invocation", Type: "int"},
            {Name: "min-old-versions", Type: "int"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{
                Lambda:     lambda.NewFromConfig(*cfg),
                CloudWatch: cloudwatch.NewFromConfig(*cfg),
            }
        },
    }, (*AWSCommand).executeLambda)
},
```

Delete `runLambdaCmd`. Run `goimports -w cmd/lambda.go`.

- [ ] **Step 2: Convert `cmd/logs.go`**

Replace `logsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{CWL: cloudwatchlogs.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeLogs)
},
```

Delete `runLogsCmd`. Run `goimports -w cmd/logs.go`.

- [ ] **Step 3: Convert `cmd/natgateways.go`**

Replace `natgatewaysCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-state", Type: "string"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeNatGateways)
},
```

Delete `runNatGatewaysCmd`. Run `goimports -w cmd/natgateways.go`.

- [ ] **Step 4: Convert `cmd/opensearch.go`**

Replace `opensearchCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "days-no-indexing", Type: "int"},
            {Name: "hours-no-searches", Type: "int"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{
                OpenSearch: opensearch.NewFromConfig(*cfg),
                CloudWatch: cloudwatch.NewFromConfig(*cfg),
            }
        },
    }, (*AWSCommand).executeOpenSearch)
},
```

Delete `runOpenSearchCmd`. Run `goimports -w cmd/opensearch.go`.

- [ ] **Step 5: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 6: Smoke-test one command**

Run: `go run . lambda --help`
Expected: usage block with `--days-since-invocation` and `--min-old-versions`.

- [ ] **Step 7: Commit**

```bash
git add cmd/lambda.go cmd/logs.go cmd/natgateways.go cmd/opensearch.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert lambda/logs/natgateways/opensearch to shared runner

Collapses each RunE to a runResourceCommand call and deletes the
per-file wrapper functions. executeX methods and their helpers are
unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Convert batch 5 — rds, route53zones, s3buckets, snapshots

**Files:** Modify: `cmd/rds.go`, `cmd/route53zones.go`, `cmd/s3buckets.go`, `cmd/snapshots.go`

### Steps

- [ ] **Step 1: Convert `cmd/rds.go`**

Replace `rdsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "include-instances", Type: "bool"},
            {Name: "include-snapshots", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{RDS: rds.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeRDS)
},
```

Delete `runRDSCmd`. Run `goimports -w cmd/rds.go`.

- [ ] **Step 2: Convert `cmd/route53zones.go`**

Replace `route53ZonesCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "include-non-empty", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{R53: route53.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeRoute53Zones)
},
```

Delete `runRoute53ZonesCmd`. Run `goimports -w cmd/route53zones.go`.

- [ ] **Step 3: Convert `cmd/s3buckets.go`**

(Task 2 already moved `CloudConfig` onto `AWSCommand` — the runner's `newAWSCommand` populates it, so `executeS3Buckets` and its helpers will read `a.CloudConfig` correctly.)

Replace `s3bucketsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "check-lifecycle", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{S3: s3.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeS3Buckets)
},
```

Delete `runS3BucketsCmd`. Run `goimports -w cmd/s3buckets.go`.

- [ ] **Step 4: Convert `cmd/snapshots.go`**

Replace `snapshotsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeSnapShot)
},
```

Delete `runSnapshotCmd`. Run `goimports -w cmd/snapshots.go`. (Note: the method is named `executeSnapShot` with capital 'S' in 'Shot' — that's the existing spelling; preserve it.)

- [ ] **Step 5: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 6: Smoke-test one command**

Run: `go run . s3buckets --help`
Expected: usage block with `--check-lifecycle`.

- [ ] **Step 7: Commit**

```bash
git add cmd/rds.go cmd/route53zones.go cmd/s3buckets.go cmd/snapshots.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert rds/route53zones/s3buckets/snapshots to shared runner

Collapses each RunE to a runResourceCommand call and deletes the
per-file wrapper functions. s3buckets relies on CloudConfig now living
on AWSCommand (Task 2); other executeX methods are unchanged.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Convert batch 6 — targetgroups, tgwattachments, vpcendpoints, vpnconnections

**Files:** Modify: `cmd/targetgroups.go`, `cmd/tgwattachments.go`, `cmd/vpcendpoints.go`, `cmd/vpnconnections.go`

### Steps

- [ ] **Step 1: Convert `cmd/targetgroups.go`**

Replace `targetgroupsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-name", Type: "string"},
            {Name: "include-attached", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{
                ELB: elasticloadbalancingv2.NewFromConfig(*cfg),
                EC2: ec2.NewFromConfig(*cfg),
            }
        },
    }, (*AWSCommand).executeTargetGroups)
},
```

Delete `runTargetGroupsCmd`. Run `goimports -w cmd/targetgroups.go`.

- [ ] **Step 2: Convert `cmd/tgwattachments.go`**

Replace `tgwAttachmentsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "include-associated", Type: "bool"},
            {Name: "include-non-vpc", Type: "bool"},
            {Name: "filter-by-resource", Type: "string"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeTGWAttachments)
},
```

Delete `runTGWAttachmentsCmd`. Run `goimports -w cmd/tgwattachments.go`.

- [ ] **Step 3: Convert `cmd/vpcendpoints.go`**

Replace `vpcEndpointsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-service", Type: "string"},
            {Name: "include-attached", Type: "bool"},
            {Name: "include-non-interface", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeVPCEndpoints)
},
```

Delete `runVPCEndpointsCmd`. Run `goimports -w cmd/vpcendpoints.go`.

- [ ] **Step 4: Convert `cmd/vpnconnections.go`**

Replace `vpnConnectionsCmd.RunE` body with:

```go
RunE: func(cmd *cobra.Command, args []string) error {
    return runResourceCommand(cmd, CommandSetup{
        AdditionalFlags: []flags.Flag{
            {Name: "filter-by-id", Type: "string"},
            {Name: "include-up", Type: "bool"},
        },
        BuildClients: func(cfg *aws.Config) *handlers.AWSClientImpl {
            return &handlers.AWSClientImpl{EC2: ec2.NewFromConfig(*cfg)}
        },
    }, (*AWSCommand).executeVPNConnections)
},
```

Delete `runVPNConnectionsCmd`. Run `goimports -w cmd/vpnconnections.go`.

- [ ] **Step 5: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 6: Smoke-test one command**

Run: `go run . targetgroups --help`
Expected: usage block with `--filter-by-name` and `--include-attached`.

- [ ] **Step 7: Commit**

```bash
git add cmd/targetgroups.go cmd/tgwattachments.go cmd/vpcendpoints.go cmd/vpnconnections.go
git commit -m "$(cat <<'EOF'
refactor(cmd): convert targetgroups/tgwattachments/vpcendpoints/vpnconnections to shared runner

Collapses each RunE to a runResourceCommand call and deletes the
per-file wrapper functions. executeX methods and their helpers are
unchanged. This completes the conversion of all 25 command files.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Final verification and cleanup

**Why:** Confirm no residual wrappers remain, every command still registers its flags, full test suite passes, and the total LOC reduction is roughly what we predicted (~1,500 LOC).

### Steps

- [ ] **Step 1: Confirm no `run*Cmd` wrappers remain**

Run: `grep -rn "^func run.*Cmd(" cmd/ || echo "none found"`
Expected: `none found` (zero matches).

- [ ] **Step 2: Full build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: success.

- [ ] **Step 3: Top-level help**

Run: `go run . --help`
Expected: root usage block listing all 25 subcommands (volumes, snapshots, images, elasticips, enis, targetgroups, elbv1, elbv2, autoscaling, natgateways, rds, logs, efs, ecr, route53zones, vpcendpoints, clientvpn, vpnconnections, tgwattachments, lambda, elasticache, opensearch, dynamodb, s3buckets, ecs).

- [ ] **Step 4: Spot-check subcommand help for three commands**

Run each and confirm flags print:
```
go run . autoscaling --help
go run . s3buckets --help
go run . elbv1 --help
```
Expected: each shows the correct command-specific flags plus the six global persistent flags.

- [ ] **Step 5: Confirm the runner test still passes in isolation**

Run: `go test ./cmd/ -run TestRunResourceCommand_HappyPath -v`
Expected: PASS.

- [ ] **Step 6: Measure LOC reduction**

Run: `wc -l cmd/*.go | tail -1`
Record the number. Compare to the pre-refactor baseline (7,597 LOC). Expected reduction: ~1,200–1,500 LOC across the 25 files. If the reduction is drastically smaller (<500 LOC) or larger (>2,500 LOC), investigate — something was missed or deleted that shouldn't have been.

- [ ] **Step 7: Run pre-commit hooks on the working tree**

Run: `pre-commit run --all-files` (if available).
Expected: all hooks pass.

If any hook complains (e.g. `goimports` wants to rewrite a file that was missed), fix inline and amend the most recent relevant commit.

- [ ] **Step 8: No commit needed unless fixes were applied in Step 7**

If Step 7 surfaced fixes, stage them and create a follow-up commit:

```bash
git add -u cmd/
git commit -m "$(cat <<'EOF'
style(cmd): apply goimports cleanup missed during conversion

Post-refactor lint pass after the runResourceCommand conversion.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

Otherwise, the refactor is done — all 11 commits land cleanly.
