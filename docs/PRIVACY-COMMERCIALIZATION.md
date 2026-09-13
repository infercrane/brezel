# Privacy and commercialization boundary

> Engineering analysis, not legal advice. Counsel should review the release,
> dependency inventory, trademark use, and customer terms before launch.

## Can we sell a product built on E2B Runtime?

Yes. The reviewed E2B Runtime revision declares Apache License 2.0. That license
permits commercial use, modification, hosting, sublicensing, and distribution.
It does not require us to publish private modifications merely because customers
use a hosted service.

Viable products include:

- a metered managed runtime;
- a paid self-hosted enterprise distribution;
- dedicated or customer-cloud deployments;
- support, security maintenance, upgrades, and an SLA; and
- separately licensed management, compliance, and fleet-operation services.

The license does not transfer the E2B trademark or guarantee rights in every
dependency, image, binary, or third-party patent. A distributed product must:

1. preserve the upstream license and required notices;
2. retain relevant copyright, patent, trademark, and attribution notices;
3. identify modified upstream files;
4. inventory and comply with every bundled dependency and image license; and
5. use our own product name and marks, with E2B mentioned only as factual
   compatibility or attribution.

The project should maintain `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, a
machine-readable SBOM, source revision manifests, and modification records in
every release. Warranties, indemnities, and support are offered by this project,
not on behalf of upstream contributors.

If E2B changes the license later, Apache 2.0 rights to a revision already received
do not disappear. We could keep using that revision, but would own future security
maintenance or need to negotiate access to later releases.

## What self-hosted must mean

Running the control plane in a customer's VPC is not by itself a privacy
guarantee. The product may claim a private or air-gapped profile only when the
release passes a no-unapproved-egress conformance test.

The reviewed E2B Embed profile currently references container images in
`us-docker.pkg.dev/e2b-artifacts` and kernel, Firecracker, BusyBox, and `envd`
artifacts in `storage.googleapis.com/e2b-artifact-binaries`. Those are reasonable
upstream defaults, but a production private distribution must rebuild or verify,
sign, and mirror every artifact into a registry controlled by us or the customer.

The reviewed API also constructs a PostHog client and enqueues lifecycle events.
With a blank key, its implementation silences logs rather than selecting an
explicit no-op client. We must not interpret that as proof of zero telemetry.
Before release we will:

- replace analytics with an explicit `off | local | custom` setting;
- make `off` the private-profile default and implement it with a true no-op;
- send OpenTelemetry only to a customer-configured local endpoint;
- deny external control-plane egress except during an approved update operation;
- packet-test the complete stack with DNS and outbound connections recorded; and
- publish the allowlist needed by each deployment profile.

## Customer data boundaries

Customer source, prompts, model responses, terminal streams, browser state,
artifacts, snapshots, and volumes are content. They are not product analytics.
Content collection is off by default and requires a separate, tenant-scoped
customer policy.

Operational telemetry may contain resource IDs, timings, byte counts, status,
policy decisions, and redacted failure classes. It must not contain command text,
file contents, prompts, model responses, credentials, or customer-provided URL
query strings.

Snapshots need the strongest treatment because they can capture process memory,
tokens, browser sessions, and files. The platform therefore requires:

- per-tenant encryption scopes and customer-managed-key support;
- tenant-bound object names and authorization on create, resume, fork, and delete;
- independent retention for snapshots, volumes, logs, artifacts, and backups;
- durable deletion reconciliation and a deletion audit record;
- credential revocation or rebinding across pause, resume, and fork; and
- support-bundle redaction with customer approval before export.

A private model route does not imply that all model calls stay local. The UI and
API must identify whether a route targets a customer-hosted model, our managed
model, or an external provider. The selected route determines the data processor
and must be visible in policy and audit records.

## Security and privacy features we own

E2B Runtime provides a strong substrate, but the product promise is the qualified
distribution and policy plane around it.

### Release 1: trustworthy private runtime

- reproducible, pinned builds with our own signed artifact mirror;
- SBOM, provenance, vulnerability scanning, patch-lag policy, and rollback;
- explicit zero-telemetry mode and no-unapproved-egress conformance test;
- encrypted snapshots, volumes, logs, artifacts, and backups;
- retention and deletion policies with customer-managed keys;
- deny-by-default sandbox network policy and cloud-metadata protection;
- a production installer, preflight checks, backup, restore, and upgrade path; and
- a single-tenant private deployment profile with honest assurance labels.

### Release 2: credentials, jobs, and model routes

- an open credential broker that never places long-lived secrets in a guest;
- Vault, AWS, GCP, and Azure identity integrations;
- short-lived endpoint-bound credentials and domain, method, path, quota, and
  spend policy;
- durable jobs with fan-out, concurrency, retry, cancellation, scheduling, and
  result collection;
- aliases for approved vLLM, SGLang, TGI, NIM, or OpenAI-compatible endpoints;
- route-specific token, concurrency, cost, and data-residency controls; and
- framework adapters for E2B SDK users, OpenAI Agents, Anthropic, and MCP.

### Release 3: enterprise fleet

- OIDC, SAML, SCIM, service accounts, RBAC/ABAC, quotas, and approval workflows;
- multi-cluster capacity management and safe regional placement;
- private ingress, private endpoints, static egress, and offline update bundles;
- customer-owned observability with content-minimal lifecycle and cost metrics;
- versioned shared agent workspaces after filesystem correctness qualification;
- signed execution receipts for image, policy, route, lifecycle, and cleanup; and
- externally reviewed shared-multitenant and dedicated-accelerator profiles.

## Open-core boundary

Keep the security fundamentals open. The Apache-licensed core should include the
runtime distribution, single-cluster control plane, network policy, credential
broker, jobs, model routes, lifecycle audit, and conformance suite. This makes the
privacy claim inspectable and prevents the community edition from being unsafe by
design.

Sell operational leverage:

- managed usage;
- hardened long-term-support releases;
- fleet and multi-region management;
- enterprise identity and governance integrations;
- air-gapped update channels and compliance evidence packs;
- dedicated capacity, customer-cloud operations, and premium support; and
- later, co-placement of agent execution with private inference.

This is more defensible than selling a thin E2B wrapper. The product becomes the
private agent execution system enterprises can operate, govern, and connect to
their own models.

## Release gate

No page or contract may say `air-gapped`, `zero telemetry`, `data never leaves`,
`hostile multi-tenant`, or `compliant` until the exact deployment profile has an
automated conformance result and, where appropriate, independent review. Legal
permission to use Apache-licensed code is not evidence for any security claim.

## Primary references

- E2B Runtime repository and license: <https://github.com/e2b-dev/runtime>
- Apache License 2.0: <https://www.apache.org/licenses/LICENSE-2.0>
- Apache licensing FAQ: <https://www.apache.org/foundation/license-faq.html>

