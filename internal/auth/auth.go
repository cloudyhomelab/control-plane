// Package auth verifies GitHub Actions OIDC tokens.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/cloudyhome/controlplane/internal/policy"
)

type Verifier interface {
	Verify(ctx context.Context, rawToken string) (policy.Claims, error)
}

type OIDC struct {
	verifier   *oidc.IDTokenVerifier
	allowedOrg string
}

// NewOIDC verifies against a JWKS URL directly, so startup needs no network access.
func NewOIDC(ctx context.Context, issuer, jwksURL, audience, allowedOrg string) *OIDC {
	keys := oidc.NewRemoteKeySet(ctx, jwksURL)
	return &OIDC{
		verifier:   oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience}),
		allowedOrg: allowedOrg,
	}
}

func (o *OIDC) Verify(ctx context.Context, raw string) (policy.Claims, error) {
	tok, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var all map[string]any
	if err := tok.Claims(&all); err != nil {
		return nil, err
	}
	return checkOrg(stringClaims(all), o.allowedOrg)
}

func stringClaims(all map[string]any) policy.Claims {
	c := policy.Claims{}
	for k, v := range all {
		if s, ok := v.(string); ok {
			c[k] = s
		}
	}
	return c
}

func checkOrg(c policy.Claims, org string) (policy.Claims, error) {
	if c["repository_owner"] != org {
		return nil, fmt.Errorf("repository_owner %q is not allowed", c["repository_owner"])
	}
	if !strings.HasPrefix(c["repository"], org+"/") {
		return nil, fmt.Errorf("repository %q is outside %s", c["repository"], org)
	}
	return c, nil
}

// Dev accepts an unsigned base64url JSON object of claims. Only for loopback testing.
type Dev struct{ AllowedOrg string }

func (d Dev) Verify(_ context.Context, raw string) (policy.Claims, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("dev token: %w", err)
	}
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("dev token: %w", err)
	}
	return checkOrg(stringClaims(all), d.AllowedOrg)
}

func DevToken(claims map[string]string) string {
	b, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(b)
}

func BearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return "", errors.New("missing bearer token")
	}
	return tok, nil
}
