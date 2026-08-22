# Releasing the SDKs

The two client SDKs publish independently via tag-triggered GitHub Actions.
Server releases do **not** publish the SDKs — the tag prefixes are distinct.

| SDK | Package | Registry | Workflow | Tag |
|-----|---------|----------|----------|-----|
| Python | `mindd` | [PyPI](https://pypi.org/project/mindd/) | `.github/workflows/release-python-sdk.yml` | `python-sdk-vX.Y.Z` |
| TypeScript | `@mindd/client` | [npm](https://www.npmjs.com/package/@mindd/client) | `.github/workflows/release-typescript-sdk.yml` | `typescript-sdk-vX.Y.Z` |

## One-time setup (registry side)

You must do this once before the first release; the workflows can't self-configure it.

### PyPI (Trusted Publishing — no stored token)

The Python workflow authenticates with OIDC, so there's **no API token to store**.
On PyPI, add a *pending* Trusted Publisher (Account → Publishing) matching:

- **PyPI project name:** `mindd`
- **Owner:** `vibed-project` · **Repository:** `MemorySidecar`
- **Workflow name:** `release-python-sdk.yml`
- **Environment:** `pypi`

Repeat on [TestPyPI](https://test.pypi.org/) with environment `testpypi` if you
want to rehearse (see below).

Optionally create GitHub Environments named `pypi` and `testpypi`
(Settings → Environments) to add approval gates.

### npm (automation token + provenance)

1. Ensure the `@mindd` scope/org exists on npm and the release account is a
   publisher for it.
2. Create an **Automation** access token (or a Granular token scoped to publish
   `@mindd/*`) and add it as the repository secret **`NPM_TOKEN`**
   (Settings → Secrets and variables → Actions).
3. Provenance is already wired (`publishConfig.provenance` + `id-token: write`);
   it needs no extra secret, just the OIDC permission the workflow grants.

## Cutting a release

1. **Bump the version** in the package manifest and commit on `main`:
   - Python: `sdk/python/pyproject.toml` → `[project].version`
   - TypeScript: `sdk/typescript/package.json` → `version`
   (SemVer, independent per SDK.)
2. **Tag and push** the matching prefix:
   ```bash
   git tag python-sdk-v0.1.0      # or typescript-sdk-v0.1.0
   git push origin python-sdk-v0.1.0
   ```
3. The workflow builds, checks, and publishes. It also uploads the built
   artifact (sdist+wheel / tarball) to the run for inspection.

## Rehearsing without publishing

Both workflows support `workflow_dispatch` from the Actions tab:

- **Python** → choose `testpypi` to publish to TestPyPI (requires the TestPyPI
  Trusted Publisher above), or `pypi` for a real release.
- **TypeScript** → leave `dry_run=true` (default) to build, test, and pack
  **without** publishing; set it to `false` to publish.

## Notes

- The published Python wheel/sdist and the npm tarball are built from `main` at
  the tagged commit, so make sure the SDK sources (including regenerated proto
  stubs, `make proto-python` / `make proto-ts`) are committed before tagging.
- `twine check` (Python) and `npm pack` + `npm test` (TypeScript) run in CI
  before publish, so a malformed package fails the job rather than the registry.
