// Package githubcredential verifies the persistent Control Plane classic PAT
// without exposing its body in durable state or diagnostics.
package githubcredential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var (
	ErrRejected       = errors.New("GitHub credential rejected")
	ErrScopeDeficient = errors.New("GitHub credential lacks required scopes")
	ErrOwnerMismatch  = errors.New("GitHub credential owner binding failed")
	ErrSSOBlocked     = errors.New("GitHub credential requires organization SSO authorization")
)

type Verification struct {
	FingerprintSHA256 string    `json:"fingerprint_sha256"`
	Login             string    `json:"login"`
	UserID            int64     `json:"user_id"`
	Owner             string    `json:"owner"`
	Scopes            []string  `json:"scopes"`
	VerifiedAt        time.Time `json:"verified_at"`
}

type Verifier struct {
	APIBase string
	Client  *http.Client
	Now     func() time.Time
}

func (v Verifier) Verify(ctx context.Context, token, owner string) (Verification, error) {
	token, owner = strings.TrimSpace(token), strings.TrimSpace(owner)
	if token == "" || owner == "" {
		return Verification{}, errors.New("GitHub token and owner are required")
	}
	base := strings.TrimRight(v.APIBase, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	client := v.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/user", nil)
	if err != nil {
		return Verification{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return Verification{}, fmt.Errorf("verify GitHub credential: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Verification{}, ErrRejected
	}
	if resp.StatusCode == http.StatusForbidden {
		if resp.Header.Get("X-GitHub-SSO") != "" {
			return Verification{}, ErrSSOBlocked
		}
		return Verification{}, ErrRejected
	}
	if resp.StatusCode != http.StatusOK {
		return Verification{}, fmt.Errorf("verify GitHub credential: GitHub returned HTTP %d", resp.StatusCode)
	}
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return Verification{}, fmt.Errorf("decode GitHub identity: %w", err)
	}
	scopes := parseScopes(resp.Header.Get("X-OAuth-Scopes"))
	if !hasScope(scopes, "repo") || !hasScope(scopes, "workflow") {
		return Verification{}, ErrScopeDeficient
	}
	if !strings.EqualFold(user.Login, owner) {
		membershipURL := base + "/orgs/" + url.PathEscape(owner) + "/memberships/" + url.PathEscape(user.Login)
		membership, err := http.NewRequestWithContext(ctx, http.MethodGet, membershipURL, nil)
		if err != nil {
			return Verification{}, err
		}
		membership.Header.Set("Authorization", "Bearer "+token)
		membership.Header.Set("Accept", "application/vnd.github+json")
		membershipResp, err := client.Do(membership)
		if err != nil {
			return Verification{}, fmt.Errorf("verify GitHub owner membership: %w", err)
		}
		defer membershipResp.Body.Close()
		if membershipResp.StatusCode == http.StatusForbidden && membershipResp.Header.Get("X-GitHub-SSO") != "" {
			return Verification{}, ErrSSOBlocked
		}
		if membershipResp.StatusCode != http.StatusOK {
			return Verification{}, ErrOwnerMismatch
		}
		var state struct {
			State string `json:"state"`
			Role  string `json:"role"`
		}
		if json.NewDecoder(membershipResp.Body).Decode(&state) != nil || state.State != "active" || state.Role != "admin" {
			return Verification{}, ErrOwnerMismatch
		}
	}
	now := time.Now().UTC()
	if v.Now != nil {
		now = v.Now().UTC()
	}
	sum := sha256.Sum256([]byte(token))
	return Verification{FingerprintSHA256: hex.EncodeToString(sum[:]), Login: user.Login, UserID: user.ID, Owner: owner, Scopes: scopes, VerifiedAt: now}, nil
}

func parseScopes(header string) []string {
	seen := map[string]struct{}{}
	for _, scope := range strings.Split(header, ",") {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope != "" {
			seen[scope] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for scope := range seen {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result
}
func hasScope(scopes []string, wanted string) bool {
	for _, scope := range scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}
