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

	"github.com/cloudyhomelab/control-plane/internal/policy"
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

func (oidcAuth *OIDC) Verify(ctx context.Context, raw string) (policy.Claims, error) {
	tok, err := oidcAuth.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var all map[string]any
	if err := tok.Claims(&all); err != nil {
		return nil, err
	}
	return checkOrg(stringClaims(all), oidcAuth.allowedOrg)
}

func stringClaims(all map[string]any) policy.Claims {
	claims := policy.Claims{}
	for key, value := range all {
		if text, ok := value.(string); ok {
			claims[key] = text
		}
	}
	return claims
}

func checkOrg(claims policy.Claims, org string) (policy.Claims, error) {
	if claims["repository_owner"] != org {
		return nil, fmt.Errorf("repository_owner %q is not allowed", claims["repository_owner"])
	}
	if !strings.HasPrefix(claims["repository"], org+"/") {
		return nil, fmt.Errorf("repository %q is outside %s", claims["repository"], org)
	}
	return claims, nil
}

// Dev accepts an unsigned base64url JSON object of claims. Only for loopback testing.
type Dev struct{ AllowedOrg string }

func (dev Dev) Verify(_ context.Context, raw string) (policy.Claims, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("dev token: %w", err)
	}
	var all map[string]any
	if err := json.Unmarshal(decoded, &all); err != nil {
		return nil, fmt.Errorf("dev token: %w", err)
	}
	return checkOrg(stringClaims(all), dev.AllowedOrg)
}

func DevToken(claims map[string]string) string {
	claimsJSON, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(claimsJSON)
}

func BearerToken(request *http.Request) (string, error) {
	header := request.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || tok == "" {
		return "", errors.New("missing bearer token")
	}
	return tok, nil
}
