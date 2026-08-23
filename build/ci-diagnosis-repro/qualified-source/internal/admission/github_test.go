package admission

import (
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/onboarding"
)

func TestRuntimePolicyContinuouslyRequiresCheckoutAllowability(t *testing.T) {
	base := onboarding.RepositoryPolicy{HasIssues: true, ActionsEnabled: true, ActionsAllowed: "selected", GitHubOwnedActionsAllowed: true}
	if err := validateRuntimePolicy(base); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*onboarding.RepositoryPolicy){
		func(policy *onboarding.RepositoryPolicy) { policy.GitHubOwnedActionsAllowed = false },
		func(policy *onboarding.RepositoryPolicy) { policy.ActionsAllowed = "local_only" },
	} {
		policy := base
		mutate(&policy)
		if err := validateRuntimePolicy(policy); err == nil || !strings.Contains(err.Error(), "checkout") {
			t.Fatalf("runtime Actions drift was accepted: %#v err=%v", policy, err)
		}
	}
}
