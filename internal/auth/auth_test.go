package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type issuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewEncoder(writer).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	return &issuer{srv: srv, key: key}
}

func (testIssuer *issuer) sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := jws.CompactSerialize()
	return token
}

func TestOIDC(t *testing.T) {
	iss := newIssuer(t)
	verifier := NewOIDC(context.Background(), "https://issuer.test", iss.srv.URL, "cp", "cloudyhome")
	now := time.Now()
	good := func() map[string]any {
		return map[string]any{
			"iss":              "https://issuer.test",
			"aud":              "cp",
			"sub":              "repo:cloudyhome/infra:ref:refs/heads/main",
			"iat":              now.Unix(),
			"exp":              now.Add(5 * time.Minute).Unix(),
			"repository":       "cloudyhome/infra",
			"repository_owner": "cloudyhome",
			"ref":              "refs/heads/main",
			"run_id":           "42",
		}
	}

	claims, err := verifier.Verify(context.Background(), iss.sign(t, iss.key, good()))
	if err != nil {
		t.Fatal(err)
	}
	if claims["repository"] != "cloudyhome/infra" || claims["run_id"] != "42" {
		t.Errorf("claims = %v", claims)
	}

	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := map[string]string{
		"wrong audience": iss.sign(t, iss.key, with(good(), "aud", "other")),
		"wrong issuer":   iss.sign(t, iss.key, with(good(), "iss", "https://evil.test")),
		"expired":        iss.sign(t, iss.key, with(good(), "exp", now.Add(-time.Minute).Unix())),
		"wrong org":      iss.sign(t, iss.key, with(with(good(), "repository_owner", "evil"), "repository", "evil/infra")),
		"org mismatch":   iss.sign(t, iss.key, with(good(), "repository", "evil/infra")),
		"bad signature":  iss.sign(t, otherKey, good()),
		"garbage":        "not.a.jwt",
	}
	for name, tok := range cases {
		if _, err := verifier.Verify(context.Background(), tok); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func with(claims map[string]any, key string, value any) map[string]any {
	claims[key] = value
	return claims
}

func TestDev(t *testing.T) {
	dev := Dev{AllowedOrg: "cloudyhome"}
	claims, err := dev.Verify(context.Background(), DevToken(map[string]string{"repository": "cloudyhome/x", "repository_owner": "cloudyhome"}))
	if err != nil || claims["repository"] != "cloudyhome/x" {
		t.Fatalf("%v %v", claims, err)
	}
	if _, err := dev.Verify(context.Background(), DevToken(map[string]string{"repository": "evil/x", "repository_owner": "evil"})); err == nil {
		t.Error("expected org rejection")
	}
}
