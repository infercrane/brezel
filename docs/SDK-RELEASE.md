# SDK release

Brezel publishes `brezel-sdk` to PyPI and `@infercrane/brezel` to npm. Both
packages use the same stable version and the immutable tag `sdk-vX.Y.Z`.

## One-time registry setup

Create the GitHub environment `sdk-release`, then configure these exact trusted
publisher identities:

| Registry | Package | GitHub owner | Repository | Workflow | Environment |
| --- | --- | --- | --- | --- | --- |
| PyPI pending publisher | `brezel-sdk` | `infercrane` | `brezel` | `release-sdks.yml` | `sdk-release` |
| npm trusted publisher | `@infercrane/brezel` | `infercrane` | `brezel` | `release-sdks.yml` | `sdk-release` |

PyPI can create a new project through a pending trusted publisher. npm requires
the package to exist before its trusted publisher can be configured. For the
first npm release only, an owner of the `@infercrane` scope must authenticate
interactively and publish the reviewed tag checkout:

```console
npm login
git checkout sdk-v0.1.0
cd sdk/typescript
npm ci --ignore-scripts
npm run check && npm run typecheck && npm test
npm publish
```

Then configure npm's trusted publisher with npm CLI 11.15.0 or newer:

```console
npm trust github @infercrane/brezel \
  --repo infercrane/brezel \
  --file release-sdks.yml \
  --env sdk-release \
  --allow-publish
```

Do not store PyPI or npm write tokens in GitHub. The release workflow uses
short-lived OIDC credentials on a GitHub-hosted runner.

## Release

1. Update both SDK versions and their exported version constants.
2. Run `python3 scripts/check-sdk-versions.py` and `make check`.
3. Merge the exact release commit and let `ci` and `sdk-ci` pass.
4. Create and push the immutable annotated tag `sdk-vX.Y.Z`.
5. Run the `release-sdks` workflow from that tag.
6. Verify fresh installations from PyPI and npm before updating the README.

For the first release, publish npm interactively as described above, configure
its trusted publisher, and run the workflow with `publish_npm` disabled and
`publish_pypi` enabled. Every later release publishes both packages through
OIDC.
