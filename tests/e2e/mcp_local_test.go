//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPProtocolAndOrganizationDenial(t *testing.T) {
	type args struct {
		Org     string `json:"org"`
		Tier    string `json:"tier"`
		Service string `json:"service"`
	}
	handlers := map[string]http.Handler{}
	for _, org := range []string{"org-alpha", "org-beta"} {
		server := sdk.NewServer(&sdk.Implementation{Name: "acceptance-fake", Version: "1"}, nil)
		sdk.AddTool(server, &sdk.Tool{Name: "cluster_health"}, func(_ context.Context, _ *sdk.CallToolRequest, a args) (*sdk.CallToolResult, map[string]any, error) {
			if a.Org != org {
				return nil, nil, fmt.Errorf("organization mismatch")
			}
			return nil, map[string]any{"rows": []any{}, "truncated": false}, nil
		})
		handlers[org] = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer a":
			handlers["org-alpha"].ServeHTTP(w, r)
		case "Bearer b":
			handlers["org-beta"].ServeHTTP(w, r)
		case "":
			w.WriteHeader(401)
		default:
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	s := &suite{t: t, c: config{Endpoints: map[string]string{"mcp": server.URL + "/mcp"}, Organizations: []organization{{Name: "org-alpha"}, {Name: "org-beta"}}}, fixtures: make([]fixture, 2), tokens: map[string]string{"0/developer/false": "a", "1/developer/false": "b", "0/finance/false": "finance"}}
	s.mcpCell("developer", 0, "cluster_health", true)
	s.mcpCell("developer", 1, "cluster_health", false)
	s.mcpCell("other", 0, "cluster_health", false)
	s.mcpCell("finance", 0, "cluster_health", false)
	s.mcpCell("anonymous", 0, "cluster_health", false)
}
