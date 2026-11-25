# Aenix Enterprise Platform portal

`portal/` is a Backstage New Frontend System application with published
OpenChoreo core plugins, an AEP frontend plugin and an authenticated AEP backend
proxy. It does not depend on the unpublished custom-portal generator or portal
app package.

## Versions and build

Backstage release **1.51.0**, OpenChoreo **1.4.0-m.2** (OpenChoreo 1.4 preview),
Yarn **4.13.0**, Node **24**. Backstage packages are aligned to its release
manifest; scaffolder is pinned to 1.37.0 per the OpenChoreo installation guide.
The compatibility matrix tests the 1.3 line; the selected 1.4 prerelease still
needs deployment integration testing against your control plane.

Use Node 24 (or 22 per the matrix). Node 26 cannot build the installed
`isolated-vm` dependency. No Corepack is needed. `yarn@4` is not published under
the `yarn` npm package name; the official distribution is `@yarnpkg/cli-dist`:

```sh
cd portal
npx --yes @yarnpkg/cli-dist@4.13.0 install --immutable
npx --yes @yarnpkg/cli-dist@4.13.0 tsc
npx --yes @yarnpkg/cli-dist@4.13.0 test:plugins
node --test dev/mock.test.mjs
npx --yes @yarnpkg/cli-dist@4.13.0 build:backend
```

If the installed Node is 26, prefix commands instead with
`npx --yes --package=node@24 --package=@yarnpkg/cli-dist@4.13.0 yarn`.
`build:backend` builds and bundles the frontend too. The production Dockerfile
uses Node 24, separate build/dependency/runtime stages and the unprivileged
`node` user. From the repository root:

```sh
docker build -f portal/Dockerfile -t aep-portal:local .
```

The images workflow publishes `aep-portal`; the portal workflow runs type and
plugin tests only for changes under `portal/`. No image push is required for
local development.

`portal/openchoreo-version` is the editable release pin. After changing it, run
`yarn versions:openchoreo` and `yarn install`; commit the generated root
resolutions and lockfile. All imported OpenChoreo packages are constrained there.

## Configuration

Supply secrets through the environment or mounted config, never in the image:

| Variable | Purpose |
| --- | --- |
| `KEYCLOAK_ISSUER` | Realm issuer, e.g. `https://id.example.org/realms/aep` (no trailing slash) |
| `KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET` | Confidential browser sign-in client |
| `AUTH_SESSION_SECRET` | Stable random session secret shared by replicas |
| `OPENCHOREO_API_URL` | Control-plane API base URL including `/api/v1` |
| `OPENCHOREO_CLIENT_ID`, `OPENCHOREO_CLIENT_SECRET` | Separate catalog/service client credentials |
| `AEP_READ_API_URL`, `AEP_CHANGE_API_URL` | Authenticated upstream base URLs, without `/v1` |
| `PORTAL_BASE_URL` | Public HTTPS portal URL |
| `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_USER`, `POSTGRES_PASSWORD` | Managed PostgreSQL connection |

Set the Keycloak client redirect URI to
`https://<portal>/api/auth/openchoreo-auth/handler/frame` (development:
`http://localhost:7007/api/auth/openchoreo-auth/handler/frame`). Enable the
standard authorization-code flow, map `email`, profile and group claims into
access tokens, and configure audiences accepted by OpenChoreo and both AEP
APIs. Map groups to organization roles in the APIs, not in browser configuration.
The OpenChoreo auth module performs discovery from the issuer's
`/.well-known/openid-configuration`. It is the only installed sign-in provider.
Both auth and authz feature flags remain enabled. The catalog uses its own
client-credentials token endpoint under the same realm; override
`openchoreo.auth.tokenUrl` if the service realm differs.

For local development, export the Keycloak/OpenChoreo variables and run
`yarn start`. The default database is ephemeral SQLite. AEP upstream defaults
point at the local mock; see [the API contract](portal-api.md) for its limitations.
Use `app-config.local.yaml` for local overrides (ignored by git).

## Branding without rebuilding

Set `app.title`, `organization.name`, and `app.branding.name`, `iconLogo`,
`fullLogo`, `theme.light.primaryColor`, `theme.dark.primaryColor` in app config.
The shell and sign-in card read the configured name and logos; OpenChoreo themes
read the palette. Placeholder SVGs are served at `/aep-icon.svg` and
`/aep-logo.svg`. Replace their URLs with hosted assets or SVG data URIs in config.
Production app-backend injects frontend-visible runtime config when serving the
bundle, so restart with changed config; no frontend rebuild is needed.

## Deploy with the OpenChoreo chart

Publish the image using the repository images workflow, then set the existing
OpenChoreo chart values:

```yaml
backstage:
  image:
    repository: ghcr.io/tym83/aep-portal
    tag: <immutable-image-tag>
```

Pass the environment above through the chart's deployment env/Secret mechanism
and mount any additional Backstage config. The container command loads
`app-config.yaml` and `app-config.production.yaml`; preserve both when adding a
third config file. Route the ingress to port 7007. Provision managed PostgreSQL
and TLS/CA settings appropriate to your database. Do not enable guest auth.
Chart env/volume value names vary with chart release: inspect that release's
values rather than copying names from another version.

## Implemented surfaces and integration gaps

- Organization infrastructure shows bridge clusters/services, label names,
  ServiceClaim condition status/reason and console links.
- Resource entity cards join bindings by organization/project/resource and
  submit Restore, Rotate password and Resize as normal ChangeRequests.
- Changes is enabled for the organization's approver role, with list, fresh
  detail, requester, approvals, window, phase and optimistic approve/reject.
- An NFS `createFrontendModule` adds a Request change card to OpenChoreo entity
  overviews. In **1.4.0-m.2**, promote/rollback controls live inside whole
  Deploy/overview components, not independently replaceable action extensions.
  The module uses the explicitly supported additional-action fallback. It does
  **not** suppress the original buttons, intercept suggestions or disable
  non-production actions. Protected environment writes MUST be denied to human
  identities by OpenChoreo policy; only the organization's change service may
  write bindings there. Hiding UI controls would not enforce this boundary.
- The core OpenChoreo package is installed. Optional observability, CI and
  workflow packs are not installed; automated suggestion/FinOps integration is
  therefore not implemented. Suggestions can be submitted manually through the
  Request change dialog with reviewed operation JSON.
- The production read service is a documented integration contract, with a
  development mock supplied here. The Go change service has no list endpoint,
  so the read service must also provide organization ChangeRequest lists.
- Live Keycloak sign-in, chart deployment, backend policies and real Cozystack
  state require the corresponding running services. Mock tests do not verify
  these external integrations.
