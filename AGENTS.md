# Project conventions

Instructions for anyone — human or agent — changing this repository.

## Hard constraints

- **No third-party dependencies.** The standard library only. `go.mod` must
  contain no `require` directives. Reducing the attack surface is the point of
  this project; a dependency is never worth the convenience.
- **Go 1.19.** This is the version Debian 12 ships in its official
  repositories, and the project must build there with no extra tooling. Do not
  use APIs added after 1.19.
- **Linux terminal, one binary.** No runtime, no daemon, no config server.

## Working in this repo

- Keep it small and auditable. Prefer deleting code to adding it.
- Every capability must be general-purpose: the harness works on whatever
  repository it is pointed at, and nothing about drea itself may be compiled
  into the binary. If a feature would only ever be useful when the harness edits
  its own source, it does not belong here.
- Fail safely rather than guessing. When an input is ambiguous, return an error
  that explains how to disambiguate.
- Anything the model or the provider controls is untrusted: sanitize it before
  it reaches the terminal, and confine every path to the workspace root.
- Comments explain *why*, not *what*. Most code needs none.

## Verify before finishing

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
go test -race ./...
go build ./...
```
