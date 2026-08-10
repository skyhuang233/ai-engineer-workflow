package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	githubapi "github.com/skyhuang233/workflow/internal/github"
)

const refreshWindow = 5 * time.Minute

type credentialUnavailableError struct{}

func (*credentialUnavailableError) Error() string               { return "GitHub App credential is unavailable" }
func (*credentialUnavailableError) AuthenticationFailure() bool { return true }

var ErrCredentialUnavailable = &credentialUnavailableError{}

type Config struct {
	AppID               int64
	InstallationID      int64
	PrivateKeyPEM       []byte
	RequiredPermissions map[string]string
	APIBase             string
	Client              *http.Client
	Now                 func() time.Time
}

type DiscoveryConfig struct {
	AppID         int64
	PrivateKeyPEM []byte
	Owner         string
	Repository    string
	APIBase       string
	Client        *http.Client
	Now           func() time.Time
}

type Installation struct {
	ID              int64
	Owner           string
	AllRepositories bool
}

type APIError struct {
	Operation  string
	StatusCode int
	Message    string
	RetryAt    time.Time
}

type PermissionError struct {
	Name   string
	Actual string
	Wanted string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("GitHub App installation permission %s=%q, require at least %q", e.Name, e.Actual, e.Wanted)
}

func (*PermissionError) Is(target error) bool { return target == ErrCredentialUnavailable }

func (e *PermissionError) AuthenticationFailure() bool { return errors.Is(e, ErrCredentialUnavailable) }

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Operation, e.StatusCode, e.Message)
}

func (e *APIError) Is(target error) bool {
	return target == ErrCredentialUnavailable && (e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusNotFound || (e.StatusCode == http.StatusForbidden && e.RetryAt.IsZero()))
}

func (e *APIError) AuthenticationFailure() bool {
	return errors.Is(e, ErrCredentialUnavailable)
}

func (e *APIError) RetryAtTime() time.Time { return e.RetryAt }

type Provider struct {
	appID               int64
	installationID      int64
	privateKey          *rsa.PrivateKey
	requiredPermissions map[string]string
	apiBase             string
	client              *http.Client
	now                 func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

type installationTokenResponse struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

func NewProvider(config Config) (*Provider, error) {
	if config.AppID <= 0 || config.InstallationID <= 0 {
		return nil, errors.New("GitHub App and installation IDs must be positive")
	}
	key, err := parsePrivateKey(config.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	permissions := make(map[string]string, len(config.RequiredPermissions))
	for name, access := range config.RequiredPermissions {
		if strings.TrimSpace(name) == "" || (access != "read" && access != "write") {
			return nil, errors.New("GitHub App permissions must use non-empty names and read/write access")
		}
		permissions[name] = access
	}
	if len(permissions) == 0 {
		return nil, errors.New("GitHub App permissions are required")
	}
	apiBase := strings.TrimRight(config.APIBase, "/")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Provider{
		appID: config.AppID, installationID: config.InstallationID, privateKey: key,
		requiredPermissions: permissions, apiBase: apiBase, client: client, now: now,
	}, nil
}

func (p *Provider) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().UTC()
	if p.token != "" && now.Before(p.expiresAt.Add(-refreshWindow)) {
		return p.token, nil
	}
	appJWT, err := signAppJWT(p.appID, p.privateKey, now)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", p.apiBase, p.installationID), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+appJWT)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("create GitHub App installation token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		message := strings.TrimSpace(string(body))
		return "", &APIError{Operation: "create GitHub App installation token", StatusCode: response.StatusCode, Message: message, RetryAt: responseRetryAt(response, message, now)}
	}
	var credential installationTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&credential); err != nil {
		return "", fmt.Errorf("decode GitHub App installation token: %w", err)
	}
	if credential.Token == "" || !credential.ExpiresAt.After(now.Add(refreshWindow)) {
		return "", errors.New("GitHub App returned an invalid installation token")
	}
	if err := requirePermissions(credential.Permissions, p.requiredPermissions); err != nil {
		return "", err
	}
	p.token = credential.Token
	p.expiresAt = credential.ExpiresAt.UTC()
	return p.token, nil
}

func DiscoverInstallation(ctx context.Context, config DiscoveryConfig) (Installation, error) {
	if config.AppID <= 0 || strings.TrimSpace(config.Owner) == "" || strings.TrimSpace(config.Repository) == "" {
		return Installation{}, errors.New("GitHub App discovery requires app ID, owner, and repository")
	}
	if err := githubapi.ValidateOwnerGuardedRepositoryName(config.Repository, config.Owner); err != nil {
		return Installation{}, fmt.Errorf("GitHub App discovery repository must belong to the configured owner: %w", err)
	}
	key, err := parsePrivateKey(config.PrivateKeyPEM)
	if err != nil {
		return Installation{}, err
	}
	apiBase := strings.TrimRight(config.APIBase, "/")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	client := config.Client
	if client == nil {
		client = http.DefaultClient
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	appJWT, err := signAppJWT(config.AppID, key, now().UTC())
	if err != nil {
		return Installation{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/installation", apiBase, config.Repository), nil)
	if err != nil {
		return Installation{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+appJWT)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := client.Do(request)
	if err != nil {
		return Installation{}, fmt.Errorf("discover GitHub App installation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		message := strings.TrimSpace(string(body))
		return Installation{}, &APIError{Operation: "discover GitHub App installation", StatusCode: response.StatusCode, Message: message, RetryAt: responseRetryAt(response, message, now().UTC())}
	}
	var payload struct {
		ID                  int64  `json:"id"`
		RepositorySelection string `json:"repository_selection"`
		Account             struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Installation{}, fmt.Errorf("decode GitHub App installation: %w", err)
	}
	if payload.ID <= 0 {
		return Installation{}, fmt.Errorf("%w: live installation returned an invalid ID", ErrCredentialUnavailable)
	}
	if !strings.EqualFold(payload.Account.Login, strings.TrimSpace(config.Owner)) {
		return Installation{}, fmt.Errorf("%w: live installation owner %q does not match configured owner %q", ErrCredentialUnavailable, payload.Account.Login, strings.TrimSpace(config.Owner))
	}
	if payload.RepositorySelection != "all" {
		return Installation{}, fmt.Errorf("%w: live installation repository_selection=%q, require %q", ErrCredentialUnavailable, payload.RepositorySelection, "all")
	}
	return Installation{ID: payload.ID, Owner: payload.Account.Login, AllRepositories: true}, nil
}

func VerifyInstallation(ctx context.Context, config DiscoveryConfig, expectedInstallationID int64) (Installation, error) {
	installation, err := DiscoverInstallation(ctx, config)
	if err != nil {
		return Installation{}, err
	}
	if expectedInstallationID <= 0 || installation.ID != expectedInstallationID {
		return Installation{}, fmt.Errorf("%w: live installation ID %d does not match verified installation ID %d", ErrCredentialUnavailable, installation.ID, expectedInstallationID)
	}
	return installation, nil
}

func signAppJWT(appID int64, privateKey *rsa.PrivateKey, now time.Time) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claims)
	signed := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("GitHub App private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("GitHub App private key is not RSA PKCS#1 or PKCS#8")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GitHub App private key is not RSA")
	}
	return key, nil
}

func requirePermissions(actual, required map[string]string) error {
	for name, wanted := range required {
		got := actual[name]
		if got == wanted || (wanted == "read" && got == "write") {
			continue
		}
		return &PermissionError{Name: name, Actual: got, Wanted: wanted}
	}
	return nil
}

func responseRetryAt(response *http.Response, message string, now time.Time) time.Time {
	if response.StatusCode == http.StatusTooManyRequests {
		return retryAfter(response.Header.Get("Retry-After"), now)
	}
	if response.StatusCode != http.StatusForbidden {
		return time.Time{}
	}
	if response.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
			return time.Unix(reset, 0).UTC()
		}
		return now.Add(time.Minute).UTC()
	}
	lowerMessage := strings.ToLower(message)
	if strings.Contains(lowerMessage, "secondary rate limit") || strings.Contains(lowerMessage, "abuse detection mechanism") {
		return now.Add(time.Minute).UTC()
	}
	return retryAfter(response.Header.Get("Retry-After"), now)
}

func retryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return now.Add(time.Duration(seconds) * time.Second).UTC()
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		return parsed.UTC()
	}
	return time.Time{}
}
