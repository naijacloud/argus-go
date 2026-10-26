# Security

## Reporting a vulnerability

Email **security@naijacloud.com**. Don't open a public issue, discussion or
pull request for a security problem, and don't post a proof of concept
anywhere public until a fix has shipped.

Include the version, your Go version, a minimal snippet and what you expected
instead. We acknowledge reports within two working days, tell you our
assessment and a fix timeline, and credit you in the changelog unless you
would rather we didn't.

## Supported versions

The latest minor release receives security fixes. Before 1.0.0 that is the
latest release.

## What this SDK guarantees

These are enforced in code and covered by tests. A regression in any of them is
a security bug worth reporting:

- **The DSN's public key is the only credential the SDK holds.** It can send
  data to one Argus source and read nothing. Upload tokens (`argus_up_…`) are
  for `argus-cli` and never belong in an app.
- **Personal data is filtered before anything leaves your process.**
  - The values of keys such as password, token, secret, authorization,
    cookie, bvn, pin and card are replaced with `[Filtered]`, at any depth, in
    events, breadcrumbs, log attributes and span attributes.
  - Card numbers, bearer and basic credentials, JWTs and sensitive
    `key=value` pairs are filtered inside strings.
  - Query strings and `user:password@` are removed from URLs.
  - Cookies and request bodies are never sent.
  - The user's IP address is not sent unless you set `SendDefaultPii: true`.
- **The SDK never panics into your code, and never blocks it.** Sends are
  queued and made in the background; queues are bounded; failures are dropped.
  `Recover` re-panics on purpose, with your original value, so a crash stays a
  crash.
- **No dependencies.** The module requires nothing outside the Go standard
  library, including in its tests: every dependency is supply-chain risk in
  your build.
- **The SDK's own requests use their own HTTP transport,** so they never pass
  through a `RoundTripper` you installed, and your transports never see the
  DSN key.

## Handling the DSN

Read it from the environment (`ARGUS_DSN`). Use a separate source, and so a
separate DSN, for staging. If a key is abused, revoke it in the source's
settings in Argus: the SDK stops sending to an endpoint that answers 401 or
403 until the process restarts.
