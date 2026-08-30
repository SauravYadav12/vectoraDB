# Contributing to VectoraDB

Thanks for your interest. VectoraDB is a serverless-Postgres platform written in
Go, with a React/TypeScript web app. This guide covers how to build, test, and
propose changes.

## Development setup

The engine (ZFS + Docker + Postgres) is Linux-only and runs inside a local VM —
Lima on macOS, WSL2 on Windows. The `vdb` binary is a launcher that forwards
engine commands into that VM.

```bash
make build        # host binary -> ./bin/vdb
make vm-build     # Linux engine binary -> /tmp/vdb inside the Lima VM (macOS)
make web-dev      # run the web app against the API (http://localhost:5173)
```

## Before you open a pull request

```bash
make vet          # go vet ./...
make test         # Go unit tests (host, no VM needed)
gofmt -l cmd internal   # must print nothing
```

CI runs the same checks on Linux (`.github/workflows/ci.yml`) and Windows. For
changes to the engine lifecycle, run the end-to-end suite in the VM:

```bash
make integration
```

Please:

- keep changes focused; one logical change per PR;
- match the surrounding style — comments explain *why*, not *what*;
- add or update tests for behaviour you change;
- update docs and the README when you add or change a user-facing feature.

## Contributor License Agreement (CLA)

VectoraDB's core is **AGPL-3.0-or-later**. To keep the option of relicensing the
project in the future (for example to Apache-2.0), we ask contributors to sign a
lightweight CLA granting us the right to relicense their contribution, before we
can merge it. We will provide the CLA link on your first pull request. This is
standard practice and does not affect your own rights to your code.

## Reporting bugs and security issues

- **Bugs / features:** open a GitHub issue with steps to reproduce.
- **Security vulnerabilities:** do **not** open a public issue — see
  [SECURITY.md](SECURITY.md).

## Code of Conduct

Participation is governed by our [Code of Conduct](CODE_OF_CONDUCT.md).
