package oauth

import (
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword(hash, "wrong password entirely") {
		t.Fatal("expected wrong password to fail")
	}
}

func TestHashPasswordRejectsShort(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected error for short password")
	}
}

func TestVerifyPKCE(t *testing.T) {
	// verifier -> S256(verifier) must round-trip through verifyPKCE.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" // known RFC 7636 test vector
	if !verifyPKCE(challenge, verifier) {
		t.Fatal("expected PKCE verification to succeed for known test vector")
	}
	if verifyPKCE(challenge, "wrong-verifier") {
		t.Fatal("expected PKCE verification to fail for wrong verifier")
	}
}

func TestRedirectURIAllowedLoopbackIgnoresPort(t *testing.T) {
	registered := []string{"http://127.0.0.1:9999/callback"}
	if !redirectURIAllowed(registered, "http://127.0.0.1:54321/callback") {
		t.Fatal("expected loopback redirect to match ignoring port")
	}
	if redirectURIAllowed(registered, "http://127.0.0.1:54321/other") {
		t.Fatal("different path must not match")
	}
	if redirectURIAllowed(registered, "http://evil.example.com/callback") {
		t.Fatal("non-loopback host must not match")
	}
}

func TestValidateRedirectURI(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
	}{
		{"https://claude.ai/callback", true},
		{"http://127.0.0.1:5173/callback", true},
		{"http://localhost:5173/callback", true},
		{"http://evil.example.com/callback", false},
		{"https://claude.ai/callback#frag", false},
	}
	for _, c := range cases {
		err := validateRedirectURI(c.uri)
		got := err == nil
		if got != c.want {
			t.Errorf("validateRedirectURI(%q) = %v, want %v (err=%v)", c.uri, got, c.want, err)
		}
	}
}

func TestStoreClientAndTokenLifecycle(t *testing.T) {
	store, err := NewStore("") // in-memory
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	client := &Client{ID: "client-1", Name: "test"}
	if err := store.SaveClient(client); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}
	got, err := store.Client("client-1")
	if err != nil || got.Name != "test" {
		t.Fatalf("Client lookup failed: %v %+v", err, got)
	}

	access := &Token{
		ClientID:  "client-1",
		Scopes:    []string{ScopeRead},
		Resource:  "https://x/mcp",
		GrantID:   "g1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.SaveTokens("tok-access", access, "", nil); err != nil {
		t.Fatalf("SaveTokens: %v", err)
	}
	if _, err := store.AccessToken("tok-access"); err != nil {
		t.Fatalf("AccessToken should be found: %v", err)
	}

	n, err := store.RevokeGrant("g1")
	if err != nil || n != 1 {
		t.Fatalf("RevokeGrant: n=%d err=%v", n, err)
	}
	if _, err := store.AccessToken("tok-access"); err == nil {
		t.Fatal("expected token to be gone after revoke")
	}
}
