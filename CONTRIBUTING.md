# Contribute to Agent Runtime

Help improve an integration, fix a reproducible bug, or make a guide easier to
follow. Open an issue before a larger change so the scope and compatibility
impact are clear. Keep contributions focused on execution and host interfaces;
product-specific business rules belong in the application.

## Set up and check your change

Start with the [local quickstart](docs/quickstart.md). Use the Go version required
by [go.mod](go.mod) and Node from [.node-version](.node-version). From the repository
root, run the relevant checks:

```sh
go test ./...
```

For PostgreSQL store changes, set `AGENT_RUNTIME_TEST_POSTGRES_DSN` to a dedicated
test database before running `go test ./internal/store`. Its user must be able to
create schemas; tests isolate their tables and roll them back.

For React package changes:

```sh
cd packages/react
npm ci
npm test
```

See [CI and releases](docs/ci-cd.md) for console, manifest, and release checks.
Follow the [documentation guide](docs/documentation-guide.md) for README and guide
changes. Run changed examples and check their local links.

Report vulnerabilities through the [security policy](SECURITY.md), not public issues.

## Submit a pull request

Target `develop`. Explain the problem, the resulting behavior, and the checks you
ran. For API changes, describe compatibility with existing clients and test the
contract. Include documentation when setup or behavior changes.

Contributions are accepted under [Apache-2.0](LICENSE), section 5. Preserve
third-party licenses and attribution.
