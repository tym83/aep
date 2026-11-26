// Package octest provides an httptest fake of the OpenChoreo REST API and of
// the OAuth token endpoint, mirroring the stand-verified behavior the change
// service depends on: bearer authentication, whole-spec replacement on PUT
// and 404 on missing bindings.
package octest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type Call struct {
	Method, Path string
}

type Server struct {
	*httptest.Server
	mu       sync.Mutex
	bindings map[string]map[string]any
	// Token, when set, is the only accepted bearer token.
	Token string
	// FailWrites makes the next N mutating calls fail with 500.
	FailWrites int
	// HoldFinalizers simulates a controller that has not finished cleanup.
	HoldFinalizers bool
	Calls          []Call
}

func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{bindings: map[string]map[string]any{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// Seed stores a binding without going through HTTP.
func (s *Server) Seed(namespace, kind, name string, spec map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[key(namespace, kind, name)] = map[string]any{
		"apiVersion": "openchoreo.dev/v1alpha1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": namespace, "resourceVersion": "1"},
		"spec":       spec,
	}
}

// Binding returns the stored spec of a binding, or nil.
func (s *Server) Binding(namespace, kind, name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.bindings[key(namespace, kind, name)]
	if !ok {
		return nil
	}
	spec, _ := obj["spec"].(map[string]any)
	return spec
}

// SetSpec replaces the stored spec of a binding, simulating a foreign write.
func (s *Server) SetSpec(namespace, kind, name string, spec map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(namespace, kind, name)
	if obj, ok := s.bindings[k]; ok {
		obj["spec"] = spec
	}
}

func (s *Server) CallsMade() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.Calls...)
}

func key(ns, kind, name string) string { return ns + "/" + kind + "/" + name }

var pluralToKind = map[string]string{
	"releasebindings":         "ReleaseBinding",
	"resourcereleasebindings": "ResourceReleaseBinding",
	"projectreleasebindings":  "ProjectReleaseBinding",
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Calls = append(s.Calls, Call{Method: r.Method, Path: r.URL.Path})
	s.mu.Unlock()
	if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"), "/")
	if len(parts) < 2 {
		writeError(w, http.StatusNotFound, "unknown path")
		return
	}
	ns, plural := parts[0], parts[1]
	kind, ok := pluralToKind[plural]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown resource")
		return
	}
	name := ""
	if len(parts) > 2 {
		name = parts[2]
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		obj, ok := s.bindings[key(ns, kind, name)]
		s.mu.Unlock()
		if !ok {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, obj)
	case http.MethodPost:
		if !s.allowWrite(w) {
			return
		}
		var obj map[string]any
		if e := json.NewDecoder(r.Body).Decode(&obj); e != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		meta, _ := obj["metadata"].(map[string]any)
		newName, _ := meta["name"].(string)
		if newName == "" {
			writeError(w, http.StatusBadRequest, "metadata.name required")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		k := key(ns, kind, newName)
		if _, exists := s.bindings[k]; exists {
			writeError(w, http.StatusConflict, "already exists")
			return
		}
		meta["namespace"] = ns
		obj["kind"] = kind
		s.bindings[k] = obj
		writeJSON(w, http.StatusCreated, obj)
	case http.MethodPut:
		if !s.allowWrite(w) {
			return
		}
		var obj map[string]any
		if e := json.NewDecoder(r.Body).Decode(&obj); e != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		k := key(ns, kind, name)
		if _, exists := s.bindings[k]; !exists {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		// Metadata managed by Kubernetes survives whole-spec replacement.
		oldMeta := s.bindings[k]["metadata"]
		obj["metadata"] = oldMeta
		// The API replaces the whole spec without a precondition.
		obj["kind"] = kind
		s.bindings[k] = obj
		writeJSON(w, http.StatusOK, obj)
	case http.MethodDelete:
		if !s.allowWrite(w) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		k := key(ns, kind, name)
		if _, exists := s.bindings[k]; !exists {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		obj := s.bindings[k]
		spec, _ := obj["spec"].(map[string]any)
		if s.HoldFinalizers || (kind == "ResourceReleaseBinding" && spec["retainPolicy"] == "Retain") {
			meta := obj["metadata"].(map[string]any)
			meta["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			meta["finalizers"] = []string{"openchoreo.dev/resourcereleasebinding-cleanup"}
		} else {
			delete(s.bindings, k)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) allowWrite(w http.ResponseWriter) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailWrites > 0 {
		s.FailWrites--
		writeError(w, http.StatusInternalServerError, "injected failure")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// TokenServer fakes the OAuth token endpoint for client-credentials tests.
// It accepts any client credentials and issues StaticToken.
type TokenServer struct {
	*httptest.Server
	StaticToken string
	Requests    int
	mu          sync.Mutex
}

func NewTokenServer(t *testing.T, token string) *TokenServer {
	t.Helper()
	s := &TokenServer{StaticToken: token}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Requests++
		s.mu.Unlock()
		if e := r.ParseForm(); e != nil || r.Form.Get("grant_type") != "client_credentials" {
			writeError(w, http.StatusBadRequest, "expected client_credentials grant")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": s.StaticToken, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *TokenServer) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Requests
}
