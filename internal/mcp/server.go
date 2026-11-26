// Package mcp provides organization-scoped, read-only infrastructure evidence.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/tym83/aep/internal/operator"
	"net/http"
	"sort"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tym83/aep/internal/authn"
	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var organizations = schema.GroupVersionResource{Group: "aep.aenix.io", Version: "v1alpha1", Resource: "organizations"}

type Config struct {
	MaxRows, MaxBytes, RequestsPerMinute int
	MetricsURL                           string
}
type Server struct {
	Client   dynamic.Interface
	verifier authn.Verifier
	config   Config
	mu       sync.Mutex
	limits   map[[32]byte]*bucket
}
type bucket struct {
	limiter *rate.Limiter
	touched time.Time
}
type scope struct {
	org   string
	tiers []string
}
type contextKey struct{}
type Args struct {
	Org     string `json:"org"`
	Since   string `json:"since,omitempty"`
	Until   string `json:"until,omitempty"`
	Service string `json:"service,omitempty"`
	Tier    string `json:"tier,omitempty"`
}
type Evidence struct {
	Rows        []map[string]any `json:"rows"`
	Truncated   bool             `json:"truncated"`
	Unavailable []string         `json:"unavailable,omitempty"`
}

func New(c dynamic.Interface, v authn.Verifier, cfg Config) (*Server, error) {
	if cfg.MaxRows == 0 {
		cfg.MaxRows = 100
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 65536
	}
	if cfg.RequestsPerMinute == 0 {
		cfg.RequestsPerMinute = 60
	}
	if cfg.MaxRows < 1 || cfg.MaxRows > 1000 || cfg.MaxBytes < 1024 || cfg.MaxBytes > 1048576 || cfg.RequestsPerMinute < 1 {
		return nil, fmt.Errorf("invalid output or rate limits")
	}
	return &Server{Client: c, verifier: v, config: cfg, limits: map[[32]byte]*bucket{}}, nil
}
func (s *Server) Handler() http.Handler {
	server := sdk.NewServer(&sdk.Implementation{Name: "cozystack-mcp", Version: "0.1.0"}, nil)
	for _, name := range []string{"infra_changes", "managed_service_state", "cluster_health", "quota_and_denials"} {
		sdk.AddTool(server, &sdk.Tool{Name: name, Description: descriptions[name], Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *sdk.CallToolRequest, a Args) (*sdk.CallToolResult, Evidence, error) {
			out, err := s.query(ctx, name, a)
			return nil, out, err
		})
	}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		raw := authn.BearerToken(r.Header.Get("Authorization"))
		if raw == "" {
			http.Error(w, "bearer token required", 401)
			return
		}
		id, err := s.verifier.Verify(r.Context(), raw)
		if err != nil || id == nil {
			http.Error(w, "invalid token", 401)
			return
		}
		if !s.allow(raw) {
			http.Error(w, "rate limit exceeded", 429)
			return
		}
		sc, err := s.resolve(r.Context(), id)
		if err != nil {
			http.Error(w, "token must identify exactly one organization", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, sc)))
	})
}

var descriptions = map[string]string{
	"infra_changes":         "Infrastructure release history and events, newest first. since/until are RFC3339; default last 24 hours. Historical specs are not stored in HelmRelease status.",
	"managed_service_state": "Service evidence; service is tier/kind/name, e.g. nonprod/Postgres/orders. Includes conditions, bridge references, backups and optional PVC usage.",
	"cluster_health":        "Tenant control plane, VM and node-group conditions and quotas. tier is required. Physical host data is never exposed.",
	"quota_and_denials":     "Tier quota used/hard and quota-related FailedCreate/Forbidden events in the last 24 hours.",
}

func (s *Server) allow(raw string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	key := sha256.Sum256([]byte(raw))
	for k, b := range s.limits {
		if now.Sub(b.touched) > 10*time.Minute {
			delete(s.limits, k)
		}
	}
	b := s.limits[key]
	if b == nil {
		if len(s.limits) >= 10000 {
			return false
		}
		b = &bucket{limiter: rate.NewLimiter(rate.Limit(float64(s.config.RequestsPerMinute)/60), s.config.RequestsPerMinute)}
		s.limits[key] = b
	}
	b.touched = now
	return b.limiter.Allow()
}
func (s *Server) resolve(ctx context.Context, id *authn.Identity) (scope, error) {
	list, err := s.Client.Resource(organizations).List(ctx, metav1.ListOptions{Limit: 1000})
	if err != nil {
		return scope{}, err
	}
	if list.GetContinue() != "" {
		return scope{}, fmt.Errorf("organization registry exceeds limit")
	}
	memberships := authn.Memberships(id.Groups, []string{"developer", "platform", "admin", "auditor"})
	matches := map[string]scope{}
	for _, o := range list.Items {
		name := o.GetName()
		if operator.ValidateName(name) != nil {
			continue
		}
		matched := id.ClientID == "aep-"+name+"-sre-agent" || len(memberships[name]) > 0
		if matched {
			sc := scope{org: name}
			tiers, _, _ := nestedMap(o.Object, "spec", "tiers")
			for t := range tiers {
				sc.tiers = append(sc.tiers, t)
			}
			sort.Strings(sc.tiers)
			matches[name] = sc
		}
	}
	if len(matches) != 1 {
		return scope{}, fmt.Errorf("ambiguous organization")
	}
	for _, sc := range matches {
		return sc, nil
	}
	panic("unreachable")
}
func (s *Server) bounded(e Evidence) Evidence {
	for i := range e.Rows {
		e.Rows[i] = sanitize(e.Rows[i]).(map[string]any)
	}
	if len(e.Rows) > s.config.MaxRows {
		e.Rows = e.Rows[:s.config.MaxRows]
		e.Truncated = true
	}
	for {
		b, _ := json.Marshal(e)
		if len(b) <= s.config.MaxBytes {
			return e
		}
		e.Truncated = true
		if len(e.Rows) > 0 {
			e.Rows = e.Rows[:len(e.Rows)-1]
		} else {
			e.Unavailable = nil
			return e
		}
	}
}
