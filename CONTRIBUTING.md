## Contributing

Thank you for your interest in contributing to COR! This guide will help you understand the project structure and development patterns.

### Prerequisites

- Go 1.26 or later (per `go.mod`)
- AWS account for testing (use a sandbox/dev account, never production!)
- Familiarity with AWS SDK for Go v2
- Understanding of Cobra CLI framework

### Build

```bash
go build -o cor .
```

### Tests

```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run specific package tests
go test ./pkg/utils/...
```

### Project Architecture

```
/cor/
├── cmd/                          # Command implementations (one file per resource type)
│   ├── root.go                   # Root command and AWSCommand struct
│   ├── helpers.go                # Shared utilities for commands
│   ├── volumes.go, snapshots.go  # Individual resource commands
│   └── ...
├── pkg/handlers/                 # Core logic handlers
│   ├── aws/                      # AWS client configuration & wrappers
│   ├── flags/                    # Flag parsing & precedence handling
│   ├── logging/                  # Structured logging
│   ├── printer/                  # Table printing (streaming + buffered)
│   └── prompter/                 # User interaction prompts
├── pkg/utils/                    # Utility functions
│   ├── extentions.go             # Generic slice operations
│   ├── cache.go                  # Instance existence caching
│   └── ...
├── main.go                       # Entry point with optional pprof support
└── go.mod                        # Dependencies
```

### Code Patterns & Standards

#### Concurrency Pattern

All commands should use `errgroup.WithContext` for concurrent AWS API calls:

```go
import "golang.org/x/sync/errgroup"

g, ctx := errgroup.WithContext(ctx)

// Producer goroutine
itemsChan := make(chan Item, 50)
g.Go(func() error {
    defer close(itemsChan)
    // Paginate through AWS resources
    paginator := service.NewListPaginator(client, input)
    for paginator.HasMorePages() {
        page, err := paginator.NextPage(ctx)
        if err != nil {
            return err
        }
        for _, item := range page.Items {
            select {
            case itemsChan <- item:
            case <-ctx.Done():
                return ctx.Err()
            }
        }
    }
    return nil
})

// Worker goroutines
const numWorkers = 5
for i := 0; i < numWorkers; i++ {
    g.Go(func() error {
        for item := range itemsChan {
            // Process item
            if err := processItem(ctx, item); err != nil {
                return err
            }
        }
        return nil
    })
}

if err := g.Wait(); err != nil {
    return err
}
```

**Benefits:**
- Automatic error propagation
- Context cancellation on first error
- Clean goroutine management

#### Streaming Output

Use `printer.StreamTable` for memory-efficient output:

```go
import "github.com/pincher95/cor/pkg/handlers/printer"

t := printer.NewStreamTable(output, headers, sortBy, sortDesc, 200) // 200 = flush every 200 rows
defer t.Flush()

for resource := range resources {
    t.AddRow([]any{
        resource.ID,
        resource.Name,
        resource.Size,
    })
}
```

**Benefits:**
- Constant memory usage regardless of result count
- Optional sorting (buffers all rows if enabled)
- Clean output formatting

#### Error Handling

Use structured logging with context:

```go
if err != nil {
    logger.LogError("failed to describe load balancers", err, map[string]any{
        "region": region,
        "count": len(lbs),
    }, false)
    return err
}
```

Always use safe AWS SDK accessors:

```go
// Good
name := aws.ToString(resource.Name)
size := aws.ToInt32(resource.Size)

// Bad - can panic!
name := *resource.Name
size := *resource.Size
```

### Adding a New Resource Type

Follow these steps to add support for a new orphaned resource type:

1. **Create command file** in `cmd/` (e.g., `cmd/lambda.go`)

2. **Define orphan criteria** - What makes this resource "orphaned"?
   - Not used/referenced by other resources
   - No activity for X days
   - Zero connections/invocations
   - Specific state (e.g., stopped, disabled)

3. **Implement command structure:**

```go
package cmd

import (
    "context"
    "os"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/service/lambda"
    "github.com/spf13/cobra"
    // ... other imports
)

func init() {
    addSubcommandsPallets(ParseLambdaCommand())
}

func ParseLambdaCommand() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "lambda",
        Short: "List orphaned Lambda functions",
        Long:  "Finds Lambda functions that haven't been invoked in X days",
        RunE:  runLambdaCommand,
    }

    // Add resource-specific flags
    cmd.Flags().Int("days-since-invocation", 90, "Days since last invocation")

    return cmd
}

func runLambdaCommand(cmd *cobra.Command, args []string) error {
    // Standard initialization pattern
    ctx := context.Background()

    flagRetriever := &flags.CommandFlagRetriever{Cmd: cmd}
    additionalFlags := []flags.Flag{
        {Name: "days-since-invocation", Type: "int"},
    }
    flagValues, err := flags.GetFlags(flagRetriever, additionalFlags)
    if err != nil {
        return err
    }

    // Initialize AWS clients, logger, prompter
    // ... (see existing commands for pattern)

    awsCmd := &AWSCommand{
        AWSClient: client,
        Logger:    logger,
        Prompter:  p,
        Output:    os.Stdout,
    }

    return awsCmd.executeLambda(ctx, flagValues)
}

func (e *AWSCommand) executeLambda(ctx context.Context, flags *map[string]any) error {
    // Main business logic
    // 1. List resources using pagination
    // 2. Filter orphaned resources
    // 3. Display in table format
    // 4. Optionally delete with confirmation
}
```

4. **Add to root.go** if needed (check `addSubcommandsPallets()`)

5. **Update README.md** with new resource type

6. **Update `cor.yaml`** example config if command has specific flags

7. **Write tests** for the new command

8. **Test manually** in AWS sandbox account:
   - Create test resources
   - Verify detection works
   - Test delete functionality (carefully!)
   - Test with various flag combinations

### Testing Guidelines

- **Unit tests** for utilities and helpers (100% coverage expected)
- **Integration tests** with AWS SDK mocks where possible
- **Manual testing** in sandbox AWS account for new commands
- **Never test destructive operations in production!**

### Code Review Checklist

Before submitting a PR, ensure:

- [ ] Code follows existing patterns (errgroup, streaming output, error handling)
- [ ] No commented-out code
- [ ] AWS SDK calls use safe accessors (`aws.ToString`, etc.)
- [ ] Proper nil checks before dereferencing pointers
- [ ] Structured logging with context
- [ ] Tests added for new functionality
- [ ] README.md updated if adding new resource type
- [ ] No secrets or credentials in code
- [ ] `go fmt` applied
- [ ] All tests pass: `go test ./...`

### Performance Considerations

- Use streaming output (`StreamTable`) for potentially large result sets
- Implement caching for repeated API calls (see `pkg/utils/cache.go`)
- Use appropriate channel buffer sizes (typically 50-100)
- Respect AWS API rate limits with exponential backoff (SDK handles this)
- Avoid buffering all results unless sorting is required

### Security

- Never commit AWS credentials or secrets
- Always prompt before destructive operations
- Validate user input (flags, config values)
- Use least privilege IAM permissions in examples
- Sanitize resource names/IDs in logs

### License

By contributing, you agree that your contributions will be licensed under the Apache License 2.0.
