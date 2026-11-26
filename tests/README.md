# Acceptance tests (incomplete)

[API harness](e2e/) · [Manual UX plan](ux/README.md)

Run the local harness checks with:
```sh
go test -tags e2e ./tests/e2e
```

Copy e2e/config.example.yaml to a private file, populate it, then run:
```sh
go test -v -tags e2e ./tests/e2e -config /absolute/path/config.yaml
```

The token command receives the username as its final argument and must print
only an access token. It is executed directly, without a shell. Never commit
tokens, passwords, stand addresses or private configuration. Tokens and response
bodies are excluded from test output. Redirects are not followed.

Current automated coverage is a **subset of scenario 10**: organization reads
through OpenChoreo and the read API, including forbidden response-body checks.
No scenario in DESIGN §0.1 is completely covered by this initial harness.
Stage 6 acceptance is not achieved. Missing: mutation matrix, role journeys,
portal authentication/proxy checks, MCP, network and kubectl isolation, browser
tests and manual CI workflow. No stand requests were made during development.

Matrix sources: internal/operator/openchoreo.go RenderAuthz,
internal/readapi/server.go, compliance.go and costs.go. Successful reads require
healthy services and a configured cost ledger; 503 is a failure, not authorization
success. The services endpoint currently admits finance membership, contrary to
the requested finance-only-costs journey.

Pending expectations: production log access is organization policy; finance has
no generated OpenChoreo role; breakglass grants are unimplemented. These are
printed separately and never counted as passes.

Unresolved lifecycle constraint: user deletion of projects/components/resources
is denied by current operator roles. ChangeRequest has no delete endpoint.
A self-cleaning creation journey cannot be implemented honestly without a
supported teardown contract. Production cleanup must use an approved
decommission change. Do not substitute privileged Kubernetes deletion.
