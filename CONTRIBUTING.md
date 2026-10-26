# Contributing

## Requirements

Go 1.22 or newer. Nothing else: the module has no dependencies, and neither do
its tests.

## Build and test

```sh
go build ./...          # the SDK and examples/
go vet ./...
go test -race ./...
```

The tests run a stand-in Argus on loopback with `net/http/httptest`, so they
pass with no network access. They must stay that way: a test that reaches the
internet fails in CI on the day the internet has a bad afternoon.

Formatting is checked in CI:

```sh
gofmt -l .   # must print nothing
```

CI also checks that `go mod tidy` leaves `go.mod` alone and `go.sum` empty.

## The contract test

`contract_test.go` sends one of each signal to a real Argus and checks every
endpoint answered 2xx. It is behind the `contract` build tag, so it never runs
in the normal suite:

```sh
ARGUS_CONTRACT_DSN='https://<key>@argus-in.naijacloud-staging.com/<id>' \
  go test -tags contract -run TestContract -v .
```

Use a test source's DSN on staging. The cron check-in needs a monitor with the
slug `argus-go-contract`; without one it answers 404, which the test reports
and does not fail. Set `ARGUS_CONTRACT_API` and `ARGUS_CONTRACT_TOKEN` too to
wait for the error to appear as an issue.

## Where the rules come from

The wire format, transport rules and privacy defaults are fixed by the Argus
SDK contract, `argus.sdk/1` (`spec/` in `nc-argus`), which every Argus SDK
implements. A customer moving from the JavaScript SDK to this one should
rewrite syntax, not behaviour.

If this SDK and the contract disagree, that is a bug here. If the contract and
Argus disagree, Argus wins and the contract gets fixed. Don't change behaviour
in this repo alone.

## Changes that need a test

- Anything in the transport: queues, retries, rate limits, refused keys.
- Anything in scrubbing, or in what an event, log, span or metric carries.

New behaviour without a test against the stand-in server will not be merged.

## Releasing

A Go module has no registry upload: the tag is the release.

1. Update `Version` in `client.go` (it is sent as `sdk.version`).
2. Move the `Unreleased` changelog entries under `## [x.y.z] - YYYY-MM-DD` and
   update the link definitions at the foot of the file.
3. Tag and push: `git tag v<version> && git push origin v<version>`.

`.github/workflows/release.yml` runs CI, refuses a tag that disagrees with
`Version` or has no changelog section, creates the GitHub release from the
changelog section, and asks proxy.golang.org for the tag so it is installable
at once. Never move or delete a tag after it is pushed: the proxy and the
checksum database remember the first content they saw.

## Pull requests

- One change per pull request.
- Update `CHANGELOG.md` under `## [Unreleased]`.
- Exported identifiers need doc comments in the standard Go form.
- Comments explain why. A comment restating the code is noise; a comment naming
  the failure a line prevents is worth keeping.
- Never commit a real DSN or upload token. Tests use the key
  `00000000000000000000000000000000`.
