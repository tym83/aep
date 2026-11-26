// Package authn verifies OIDC bearer tokens and extracts the caller
// identity. The change API trusts only the token: the requester of a change
// and the author of a decision always come from here, never from a body.
package authn

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Identity is the authenticated caller.
type Identity struct {
	ClientID string
	Subject  string
	Username string
	Groups   []string
}

// Verifier verifies a raw bearer token.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*Identity, error)
}

// OIDCVerifier verifies JWTs against the issuer's JWKS.
type OIDCVerifier struct {
	verifier      *oidc.IDTokenVerifier
	usernameClaim string
	groupsClaim   string
}

// NewOIDCVerifier builds a verifier for the given issuer. When audience is
// empty the client-id check is skipped; jwksURL defaults to the Keycloak
// realm certs endpoint of the issuer.
func NewOIDCVerifier(ctx context.Context, issuer, jwksURL, audience, usernameClaim, groupsClaim string) (*OIDCVerifier, error) {
	if issuer == "" {
		return nil, fmt.Errorf("issuer is required")
	}
	if jwksURL == "" {
		jwksURL = strings.TrimSuffix(issuer, "/") + "/protocol/openid-connect/certs"
	}
	if usernameClaim == "" {
		usernameClaim = "preferred_username"
	}
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	keySet := oidc.NewRemoteKeySet(ctx, jwksURL)
	cfg := &oidc.Config{ClientID: audience, SkipClientIDCheck: audience == ""}
	return &OIDCVerifier{verifier: oidc.NewVerifier(issuer, keySet, cfg), usernameClaim: usernameClaim, groupsClaim: groupsClaim}, nil
}

func (v *OIDCVerifier) Verify(ctx context.Context, rawToken string) (*Identity, error) {
	token, e := v.verifier.Verify(ctx, rawToken)
	if e != nil {
		return nil, e
	}
	var claims map[string]any
	if e := token.Claims(&claims); e != nil {
		return nil, e
	}
	username, _ := claims[v.usernameClaim].(string)
	if username == "" {
		username, _ = claims["email"].(string)
	}
	if username == "" {
		username = token.Subject
	}
	var groups []string
	switch raw := claims[v.groupsClaim].(type) {
	case []string:
		groups = raw
	case []any:
		for _, g := range raw {
			if s, ok := g.(string); ok {
				groups = append(groups, s)
			}
		}
	}
	clientID, _ := claims["client_id"].(string)
	return &Identity{ClientID: clientID, Subject: token.Subject, Username: username, Groups: groups}, nil
}

// BearerToken extracts the bearer token from an Authorization header.
func BearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}
