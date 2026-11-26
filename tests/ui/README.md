# Browser acceptance

Requires Node 22, a disposable two-organization stand and real OIDC users.
Versions and the npm lockfile are pinned independently of the portal workspace.

```sh
cd tests/ui
npm ci --ignore-scripts
npx playwright install chromium
npx tsc --noEmit
# Export the private environment described below, then:
npm test
```

Required environment variables:

| Variable | Value |
|---|---|
| `AEP_PORTAL_URL` | Portal origin |
| `AEP_DEVELOPER_USER`, `AEP_DEVELOPER_PASSWORD` | Organization A developer |
| `AEP_APPROVER_USER`, `AEP_APPROVER_PASSWORD` | Distinct organization A approver |
| `AEP_AUDITOR_USER`, `AEP_AUDITOR_PASSWORD` | Organization A auditor |
| `AEP_OWN_ENTITY`, `AEP_OWN_ENTITY_PATH` | Existing A catalog entity display name and route |
| `AEP_OTHER_ENTITY`, `AEP_OTHER_ENTITY_PATH` | Existing B entity display name and route |
| `AEP_CHANGE_URL`, `AEP_ORGANIZATION`, `AEP_PROJECT`, `AEP_PROD_ENVIRONMENT` | Change API origin and disposable A scope |
| `AEP_TOKEN_COMMAND` | JSON argv array for the developer OIDC token helper |

`AEP_SELECTORS` is an optional JSON object of Playwright selectors: `signIn`,
`username`, `password`, `submit`, `signedIn`, `infrastructure`, `entityDenied`,
`changeRow`, `approve`, `pageReady`, `createButtons`. Defaults target a popup
OIDC login with the Keycloak form (`#username`, `#password`, `#kc-login`),
Backstage navigation and the current AEP Changes list. For example:

```sh
export AEP_SELECTORS='{"signIn":"button:has-text(\"Sign In\")","infrastructure":"text=Infrastructure"}'
```

Optional route overrides: `AEP_CATALOG_PATH` and `AEP_CHANGES_PATH`.
Use an IdP session without an interactive second factor for these dedicated
acceptance identities. Login always uses the actual form, never stored browser
state or a guest bypass. Each test gets a fresh browser context. Entity fixtures
must be visible to their owning users before the run; use exact distinct names.

The approver test creates a unique, normal `decommission` request as the
developer, waits for Pending, approves it in the browser and observes the API
phase. The target is an absent resource with a random run identifier; approval
cannot provision production objects. This audit record is intentionally retained.
The API suite separately verifies real promotion and approved teardown.

Auditor checks cover the catalog, compliance and costs pages and absence of
create controls; API authorization is checked by the role matrix. A UI that
still renders unauthorized controls fails this test rather than being skipped.
Screenshots, videos, traces and persistent login state are disabled to avoid
recording credentials or connection outputs. Keep environment files private.

CI uses only [Manual acceptance](../../.github/workflows/e2e.yml), never PRs.
Configure the protected `acceptance` environment and secrets documented in the
[parent README](../README.md). `AEP_UI_ENV` is a JSON object mapping the variable
names above to string values. Helpers may reference `${E2E_DIR}` in that secret.
