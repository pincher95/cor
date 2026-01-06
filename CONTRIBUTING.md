## Contributing

### Prerequisites

- Go (per `go.mod`)

### Build

```bash
go build -o cor .
```

### Tests

```bash
go test ./...
```

### Notes

- Most commands can perform destructive actions when `--delete` is set; the CLI always prompts for confirmation.
- Configuration can be provided via `$HOME/.cor.yaml` or `--config`, and via `COR_*` environment variables.
- By contributing, you agree that your contributions will be licensed under the project license (Apache-2.0).
