# Contributing

## Before you start

- Read [AGENTS.md](AGENTS.md) — it contains the project's rules, patterns, and architecture.
- Read [LESSONS.md](LESSONS.md) for context on design decisions and pitfalls encountered.
- Follow the [DESIGN.md](DESIGN.md) principles: Go-only, stdlib-first, interface boundaries, idempotency.

## Development workflow

```bash
make all       # Run before pushing — fmt + vet + test + build
make check     # Fast pre-commit check
make test      # Unit tests only
make test-race # Unit tests with race detector
```

All code must pass `go fmt`, `go vet`, and `go test ./pkg/...` before submission.

## Code conventions

- **Section comments** use `───` separators for logical blocks within functions.
- **Log levels**: `log.Printf` for operational info, `log.Printf("Warning: ...")` for non-fatal issues, `fmt.Errorf` for errors returned to callers.
- **Package documentation** at the top of the primary file.
- **Interfaces** at package boundaries — enable mocking and testing.
- **Errors wrapped** with `%w` for error chains.

## Tests

- Unit tests in `pkg/` — use stdlib `testing`, must complete in <5s.
- Integration tests behind `//go:build integration` tag — require containerd.
- Mock implementations exist for `ContainerClient` (`pkg/cri/mock_client.go`).

## Commit messages

Keep them short and descriptive. Reference AGENTS.md lessons where relevant:

```
network: fix raw string concat for netns path (lesson #31)

Replace "/var/run/netns/" + nsName with filepath.Join to prevent
path traversal from unvalidated service names.
```

## License

Apache 2.0. All contributions are under this license.
