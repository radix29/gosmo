# Contributing to gosmo

Thanks for your interest in gosmo.

## Before you start

Open an issue to discuss a change before sending a pull request, especially a
new exported API. Read `CLAUDE.md` and `ARCHITECTURE.md` for the conventions
the code follows.

## Checks

```sh
go build ./... && go vet ./... && gofmt -l .
go test ./...
go vet -tags livedb ./...   # compiles the live-database tests too
```

`gofmt -l .` must print nothing. Tests tagged `livedb` need a SQL Server you
own and are run with `-livedb '<dsn>'`; never point them at a server you do
not own, and create only disposable objects. A change to SQL behaviour should
be exercised against a real server, not only the unit tests.

## Pull requests

- One logical change per PR, with tests for new behaviour.
- Explain what changed and why; link the issue.
- Do not edit `CHANGELOG.md` or `RELEASE.md`; the maintainer does at release.

## Issues

Bug reports, questions and feature requests are welcome. For a bug, please
include:

- the gosmo version (tag or commit) and Go version;
- the SQL Server edition and major version you connected to (on-premises,
  Azure SQL Database, Managed Instance, …);
- a minimal snippet that reproduces it, and the error or wrong output.

Never paste connection strings, passwords or tokens.

## Security

Do not report vulnerabilities in a public issue. Use GitHub's private
vulnerability reporting on the repository's Security tab.

## Licence

gosmo is MIT licensed (see `LICENSE`).
