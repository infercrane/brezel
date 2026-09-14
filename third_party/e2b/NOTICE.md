# E2B envd protocol notice

`internal/backend/e2b/wire/process.proto` is a compatibility subset of the
E2B Runtime `envd` process protocol at commit
`767ceb4b2ec0e598f512767c8da9e5e6618da368`.

Upstream: <https://github.com/e2b-dev/runtime>

The upstream work is licensed under Apache License 2.0. This repository keeps
the subset intentionally narrow so the adapter can be generated, reviewed, and
tested without importing the upstream infrastructure module and its unrelated
cloud dependencies.
