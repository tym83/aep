// Package changeapi is the HTTP front of the change service. It authenticates
// callers with OIDC bearer tokens and records ChangeRequests and human
// decisions on the management cluster. It never applies anything: execution
// belongs to the controller.
package changeapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/change"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	annotationIdempotencyKey  = "aep.aenix.io/idempotency-key"
	annotationIdempotencyHash = "aep.aenix.io/idempotency-hash"
	maxNotesPerChange         = 200
	maxNoteLength             = 4000
)

type Server struct {
	Client               client.Client
	Verifier             authn.Verifier
	OrgNamespaceTemplate string
	Now                  func() time.Time
	mux                  *http.ServeMux
}

func New(c client.Client, v authn.Verifier, orgNamespaceTemplate string) *Server {
	s := &Server{Client: c, Verifier: v, OrgNamespaceTemplate: orgNamespaceTemplate, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/changes", s.withAuth(s.createChange))
	s.mux.HandleFunc("GET /v1/changes/{org}/{name}", s.withAuth(s.getChange))
	s.mux.HandleFunc("POST /v1/changes/{org}/{name}/decision", s.withAuth(s.decide))
	s.mux.HandleFunc("POST /v1/changes/{org}/{name}/notes", s.withAuth(s.addNote))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type contextKey struct{}

func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := authn.BearerToken(r.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		id, e := s.Verifier.Verify(r.Context(), token)
		if e != nil {
			writeError(w, http.StatusUnauthorized, "the token is not valid")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, id)))
	}
}

func identity(r *http.Request) *authn.Identity {
	id, _ := r.Context().Value(contextKey{}).(*authn.Identity)
	return id
}

type createRequest struct {
	Organization  string              `json:"organization"`
	Operation     api.ChangeOperation `json:"operation"`
	Model         api.ChangeModel     `json:"model"`
	Target        api.ChangeTarget    `json:"target"`
	Desired       json.RawMessage     `json:"desired,omitempty"`
	Justification string              `json:"justification"`
	Window        *api.ChangeWindow   `json:"window,omitempty"`
	ExternalRef   string              `json:"externalRef,omitempty"`
}

type changeView struct {
	ID      string             `json:"id"`
	Request *api.ChangeRequest `json:"request"`
}

func view(cr *api.ChangeRequest) changeView {
	return changeView{ID: cr.Spec.Organization + "/" + cr.Name, Request: cr}
}

// changeName derives the deterministic object name from the idempotency
// scope: organization, requester and key. A retried request with the same
// key maps to the same object.
func changeName(org, requester, key string) string {
	sum := sha256.Sum256([]byte(org + "\x00" + requester + "\x00" + key))
	return "chg-" + hex.EncodeToString(sum[:])[:24]
}

// requestHash canonicalizes the create payload for idempotency comparison.
func requestHash(req createRequest) string {
	type alias createRequest
	normalized := alias(req)
	if len(req.Desired) > 0 {
		var anyJSON any
		if e := json.Unmarshal(req.Desired, &anyJSON); e == nil {
			if canonical, e := json.Marshal(anyJSON); e == nil {
				normalized.Desired = canonical
			}
		}
	}
	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) createChange(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	// Changes are requested by humans; adapters mirror and decide, they do
	// not originate requests on behalf of someone.
	if change.IsServiceAccount(id.Username) {
		writeError(w, http.StatusForbidden, "service accounts cannot request changes")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "the Idempotency-Key header is required")
		return
	}
	var req createRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, http.StatusBadRequest, e.Error())
		return
	}
	if len(validation.IsDNS1123Label(req.Organization)) > 0 {
		writeError(w, http.StatusBadRequest, "organization must be a DNS-1123 label")
		return
	}
	if !change.CanCreate(req.Organization, id.Groups) {
		writeError(w, http.StatusForbidden, "organization access denied")
		return
	}
	spec := api.ChangeRequestSpec{
		Organization:    req.Organization,
		Operation:       req.Operation,
		Model:           req.Model,
		Target:          req.Target,
		Justification:   req.Justification,
		Window:          req.Window,
		ExternalRef:     req.ExternalRef,
		Requester:       id.Username,
		RequesterGroups: id.Groups,
	}
	if len(req.Desired) > 0 {
		spec.Desired = &apiextensionsv1.JSON{Raw: req.Desired}
	}
	now := s.now()
	if e := change.ValidateSpec(spec, now); e != nil {
		writeError(w, http.StatusBadRequest, e.Error())
		return
	}
	if e := change.ValidateRequester(spec); e != nil {
		writeError(w, http.StatusForbidden, e.Error())
		return
	}
	hash := requestHash(req)
	cr := &api.ChangeRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      changeName(req.Organization, id.Username, key),
			Namespace: change.OrgNamespace(s.OrgNamespaceTemplate, req.Organization),
			Annotations: map[string]string{
				annotationIdempotencyKey:  key,
				annotationIdempotencyHash: hash,
			},
		},
		Spec: spec,
	}
	e := s.Client.Create(r.Context(), cr)
	if apierrors.IsAlreadyExists(e) {
		existing := &api.ChangeRequest{}
		if getErr := s.Client.Get(r.Context(), client.ObjectKeyFromObject(cr), existing); getErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot read the existing change")
			return
		}
		if existing.Annotations[annotationIdempotencyHash] != hash {
			writeError(w, http.StatusConflict, "the Idempotency-Key was already used with a different request")
			return
		}
		writeJSON(w, http.StatusOK, view(existing))
		return
	}
	if e != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist the change")
		return
	}
	writeJSON(w, http.StatusCreated, view(cr))
}

func (s *Server) readChange(w http.ResponseWriter, r *http.Request) (*api.ChangeRequest, bool) {
	org, name := r.PathValue("org"), r.PathValue("name")
	if len(validation.IsDNS1123Label(org)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
		writeError(w, http.StatusBadRequest, "invalid change id")
		return nil, false
	}
	cr := &api.ChangeRequest{}
	e := s.Client.Get(r.Context(), client.ObjectKey{Namespace: change.OrgNamespace(s.OrgNamespaceTemplate, org), Name: name}, cr)
	if apierrors.IsNotFound(e) {
		writeError(w, http.StatusNotFound, "change not found")
		return nil, false
	}
	if e != nil {
		writeError(w, http.StatusInternalServerError, "cannot read the change")
		return nil, false
	}
	return cr, true
}

func (s *Server) canRead(id *authn.Identity, cr *api.ChangeRequest) bool {
	return change.IsPlatform(id.Groups) || change.IsOrgMember(cr.Spec.Organization, id.Groups)
}

func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	cr, ok := s.readChange(w, r)
	if !ok {
		return
	}
	if !s.canRead(identity(r), cr) {
		writeError(w, http.StatusForbidden, "organization access denied")
		return
	}
	writeJSON(w, http.StatusOK, view(cr))
}

type decisionRequest struct {
	Decision string `json:"decision"`
	Evidence string `json:"evidence,omitempty"`
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	// A decision counts only when the approver confirms it with their own
	// token; there is no on-behalf-of.
	if change.IsServiceAccount(id.Username) {
		writeError(w, http.StatusForbidden, "decisions require a human token; on-behalf-of is not accepted")
		return
	}
	ifMatch := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if ifMatch == "" {
		writeError(w, http.StatusPreconditionRequired, "the If-Match header with the change resourceVersion is required")
		return
	}
	var req decisionRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, http.StatusBadRequest, e.Error())
		return
	}
	if req.Decision != change.DecisionApprove && req.Decision != change.DecisionReject {
		writeError(w, http.StatusBadRequest, "decision must be approve or reject")
		return
	}
	cr, ok := s.readChange(w, r)
	if !ok {
		return
	}
	if !s.canRead(id, cr) {
		writeError(w, http.StatusForbidden, "organization access denied")
		return
	}
	if cr.ResourceVersion != ifMatch {
		writeError(w, http.StatusPreconditionFailed, "the change was modified; re-read it and decide again")
		return
	}
	if cr.Status.Phase != api.PhasePending {
		writeError(w, http.StatusConflict, "decisions are accepted only while the change is pending; phase is "+string(cr.Status.Phase))
		return
	}
	if cr.Spec.Requester == id.Username {
		writeError(w, http.StatusForbidden, "the requester cannot decide their own change")
		return
	}
	role, eligible := change.EligibleRole(cr.Spec, id.Groups)
	if !eligible {
		writeError(w, http.StatusForbidden, "your groups grant no decision right on this change")
		return
	}
	for _, a := range cr.Status.Approvals {
		if a.User == id.Username {
			writeError(w, http.StatusConflict, "you already recorded a decision on this change")
			return
		}
	}
	cr.Status.Approvals = append(cr.Status.Approvals, api.ChangeApproval{
		User:     id.Username,
		Groups:   id.Groups,
		Role:     role,
		Decision: req.Decision,
		Time:     metav1.NewTime(s.now()),
		Source:   "portal",
		Evidence: req.Evidence,
	})
	if e := s.Client.Status().Update(r.Context(), cr); e != nil {
		if apierrors.IsConflict(e) {
			writeError(w, http.StatusPreconditionFailed, "the change was modified; re-read it and decide again")
			return
		}
		writeError(w, http.StatusInternalServerError, "cannot record the decision")
		return
	}
	writeJSON(w, http.StatusOK, view(cr))
}

type noteRequest struct {
	Text string `json:"text"`
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	var req noteRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, http.StatusBadRequest, e.Error())
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || len(req.Text) > maxNoteLength {
		writeError(w, http.StatusBadRequest, "text must be nonempty and at most 4000 characters")
		return
	}
	source := "portal"
	if change.IsServiceAccount(id.Username) {
		source = "itsm"
	}
	// Notes retry on conflict: concurrent decisions must not lose comments.
	for attempt := 0; attempt < 3; attempt++ {
		cr, ok := s.readChange(w, r)
		if !ok {
			return
		}
		if !s.canRead(id, cr) {
			writeError(w, http.StatusForbidden, "organization access denied")
			return
		}
		if len(cr.Status.Notes) >= maxNotesPerChange {
			writeError(w, http.StatusConflict, "the change has too many notes")
			return
		}
		cr.Status.Notes = append(cr.Status.Notes, api.ChangeNote{
			User: id.Username, Time: metav1.NewTime(s.now()), Text: req.Text, Source: source,
		})
		e := s.Client.Status().Update(r.Context(), cr)
		if apierrors.IsConflict(e) {
			continue
		}
		if e != nil {
			writeError(w, http.StatusInternalServerError, "cannot record the note")
			return
		}
		writeJSON(w, http.StatusCreated, view(cr))
		return
	}
	writeError(w, http.StatusConflict, "the change is being modified; retry the note")
}

func decodeBody(r *http.Request, out any) error {
	body, e := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if e != nil {
		return e
	}
	if len(body) == 0 {
		return errors.New("a JSON body is required")
	}
	if e := json.Unmarshal(body, out); e != nil {
		return errors.New("the body is not valid JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
