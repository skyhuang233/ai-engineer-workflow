package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProviderCachesInstallationTokenUntilRefreshWindow(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			t.Fatalf("installation token request = %s %s", r.Method, r.URL.Path)
		}
		assertJWTIssuer(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "123")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_token_%d", requests),
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
			"permissions": map[string]string{
				"actions": "read", "checks": "read", "contents": "write",
				"issues": "write", "metadata": "read", "pull_requests": "write",
			},
		})
	}))
	defer server.Close()

	provider, err := NewProvider(Config{
		AppID: 123, InstallationID: 42, PrivateKeyPEM: privateKey,
		RequiredPermissions: map[string]string{
			"actions": "read", "checks": "read", "contents": "write",
			"issues": "write", "metadata": "read", "pull_requests": "write",
		},
		APIBase: server.URL, Client: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "ghs_token_1" || second != first || requests != 1 {
		t.Fatalf("cached tokens = %q/%q requests=%d", first, second, requests)
	}

	now = now.Add(56 * time.Minute)
	refreshed, err := provider.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != "ghs_token_2" || requests != 2 {
		t.Fatalf("refreshed token = %q requests=%d", refreshed, requests)
	}
}

func TestDiscoverInstallationRequiresConfiguredOwnerAndAllRepositories(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/integration/installation" {
			t.Fatalf("installation discovery = %s %s", r.Method, r.URL.Path)
		}
		assertJWTIssuer(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "123")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42, "repository_selection": "all",
			"account": map[string]string{"login": "owner"},
		})
	}))
	defer server.Close()

	installation, err := DiscoverInstallation(context.Background(), DiscoveryConfig{
		AppID: 123, PrivateKeyPEM: privateKey, Owner: "owner", Repository: "owner/integration",
		APIBase: server.URL, Client: server.Client(), Now: func() time.Time { return time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.ID != 42 || installation.Owner != "owner" || !installation.AllRepositories {
		t.Fatalf("installation = %#v", installation)
	}
}

func TestDiscoverInstallationAcceptsCaseInsensitiveOwnerIdentity(t *testing.T) {
	privateKey := testPrivateKeyPEM(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/integration/installation" {
			t.Fatalf("installation discovery path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42, "repository_selection": "all",
			"account": map[string]string{"login": "OWNER"},
		})
	}))
	defer server.Close()

	installation, err := DiscoverInstallation(context.Background(), DiscoveryConfig{
		AppID: 123, PrivateKeyPEM: privateKey, Owner: "Owner", Repository: "owner/integration",
		APIBase: server.URL, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if installation.ID != 42 || installation.Owner != "OWNER" || !installation.AllRepositories {
		t.Fatalf("installation = %#v", installation)
	}
}

func TestProviderKeepsRateLimitedTokenRequestsRetryable(t *testing.T) {
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(time.Minute).Unix()))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer server.Close()
	provider, err := NewProvider(Config{
		AppID: 123, InstallationID: 42, PrivateKeyPEM: testPrivateKeyPEM(t),
		RequiredPermissions: map[string]string{"metadata": "read"},
		APIBase:             server.URL, Client: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Token(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.AuthenticationFailure() || !apiErr.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("rate-limit error = %#v", err)
	}
}

func TestProviderKeepsSecondaryRateLimitWithoutHeadersRetryable(t *testing.T) {
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer server.Close()
	provider, err := NewProvider(Config{
		AppID: 123, InstallationID: 42, PrivateKeyPEM: testPrivateKeyPEM(t),
		RequiredPermissions: map[string]string{"metadata": "read"},
		APIBase:             server.URL, Client: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Token(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.AuthenticationFailure() || !apiErr.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("secondary rate-limit error = %#v", err)
	}
}

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func assertJWTIssuer(t *testing.T, token, issuer string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT parts = %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != issuer {
		t.Fatalf("JWT issuer = %#v, want %q", claims["iss"], issuer)
	}
}
