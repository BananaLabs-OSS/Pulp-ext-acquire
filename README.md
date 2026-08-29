# Pulp-ext-acquire

`Pulp-ext-acquire` is a Pulp capability for resolving CLI binaries from an
existing installation, an operator-approved installer command, or an explicit
path.

The official-installer path assumes trusted cells because installer commands
run with the host operator's authority. See the package documentation for the
request and result contracts.

## Development

```sh
go test ./...
```

## License

MIT
