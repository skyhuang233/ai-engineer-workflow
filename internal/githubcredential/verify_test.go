package githubcredential

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifierChecksIdentityScopesAndOwner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing bearer credential")
		}
		w.Header().Set("X-OAuth-Scopes", "repo, workflow")
		_, _ = w.Write([]byte(`{"login":"SkyHuang233","id":42}`))
	}))
	defer server.Close()
	verification, err := (Verifier{APIBase: server.URL, Client: server.Client()}).Verify(context.Background(), "secret", "skyhuang233")
	if err != nil {
		t.Fatal(err)
	}
	if verification.Login != "SkyHuang233" || verification.Owner != "skyhuang233" || len(verification.Scopes) != 2 {
		t.Fatalf("verification = %#v", verification)
	}
	if len(verification.FingerprintSHA256) != 64 {
		t.Fatalf("fingerprint = %q", verification.FingerprintSHA256)
	}
}

func TestVerifierClassifiesMissingScopeWithoutLeakingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "repo")
		_, _ = w.Write([]byte(`{"login":"owner","id":1}`))
	}))
	defer server.Close()
	token := "super-secret-token"
	_, err := (Verifier{APIBase: server.URL, Client: server.Client()}).Verify(context.Background(), token, "owner")
	if !errors.Is(err, ErrScopeDeficient) {
		t.Fatalf("error = %v", err)
	}
	if err != nil && contains(err.Error(), token) {
		t.Fatal("token leaked in error")
	}
}

func TestVerifierRequiresActiveOrganizationAdminMembership(t *testing.T) {
	for _, test := range []struct {
		name       string
		membership string
		wantErr    bool
	}{
		{name: "active admin", membership: `{"state":"active","role":"admin"}`},
		{name: "active member", membership: `{"state":"active","role":"member"}`, wantErr: true},
		{name: "inactive admin", membership: `{"state":"pending","role":"admin"}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/user":
					w.Header().Set("X-OAuth-Scopes", "repo, workflow, admin:org")
					_, _ = w.Write([]byte(`{"login":"member","id":42}`))
				case "/orgs/acme/memberships/member":
					_, _ = w.Write([]byte(test.membership))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			_, err := (Verifier{APIBase: server.URL, Client: server.Client()}).Verify(context.Background(), "secret", "acme")
			if test.wantErr && !errors.Is(err, ErrOwnerMismatch) {
				t.Fatalf("membership %s error = %v", test.membership, err)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifierRequiresAdminOrgScopeBeforeOrganizationMembership(t *testing.T) {
	membershipCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("X-OAuth-Scopes", "repo, workflow")
			_, _ = w.Write([]byte(`{"login":"member","id":42}`))
		case "/orgs/acme/memberships/member":
			membershipCalled = true
			_, _ = w.Write([]byte(`{"state":"active","role":"admin"}`))
		}
	}))
	defer server.Close()
	_, err := (Verifier{APIBase: server.URL, Client: server.Client()}).Verify(context.Background(), "secret", "acme")
	if !errors.Is(err, ErrScopeDeficient) || membershipCalled {
		t.Fatalf("error=%v membership_called=%t", err, membershipCalled)
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
