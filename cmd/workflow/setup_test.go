package main

import "testing"

func TestParseGitHubRemote(t *testing.T) {
	for _, test := range []struct{ remote, want string }{
		{"git@github.com:owner/repository.git", "owner/repository"},
		{"https://github.com/owner/repository.git", "owner/repository"},
		{"ssh://git@github.com/owner/repository", "owner/repository"},
	} {
		address, ok := parseGitHubRemote(test.remote)
		if !ok || address.String() != test.want {
			t.Fatalf("parse %q = %q, %v", test.remote, address.String(), ok)
		}
	}
	if _, ok := parseGitHubRemote("https://example.com/owner/repository"); ok {
		t.Fatal("non-GitHub remote accepted")
	}
}
