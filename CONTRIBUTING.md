# Contributing

This project accepts small, reviewable changes that preserve its fail-closed
runtime contract. Start with an issue for changes to isolation, lifecycle,
credentials, networking, storage, or API semantics.

## Local checks

Use the Go version declared by `go.mod`, then run:

```bash
make check
```

Tests must not contact a paid runtime, send production traffic, or depend on a
developer's credentials. Backend fakes belong only in `_test.go` files. A test
that cannot enforce a security property must describe the missing qualification
instead of simulating success.

## Change requirements

- Add a regression test for behavior changes.
- Preserve project scope and idempotency on every resource mutation.
- Keep customer content and credentials out of logs, events, receipts, and test
  fixtures.
- Update the threat model and an ADR with changes to trust or lifecycle
  boundaries.
- State whether a capability is implemented, deployment-qualified, or only
  designed.
- Do not introduce proprietary code, copied private behavior, or unverifiable
  performance claims.

See [`AGENTS.md`](AGENTS.md), [`SECURITY.md`](SECURITY.md), and
[`docs/IMPLEMENTATION-STATUS.md`](docs/IMPLEMENTATION-STATUS.md) before making a
runtime change.
