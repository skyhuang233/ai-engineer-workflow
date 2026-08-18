package onboarding

import (
	"strings"
	"testing"
)

func TestDecideOnboardingPullFailsClosedOnIdentityOrContentDrift(t *testing.T) {
	digest := strings.Repeat("a", 64)
	baseHead := strings.Repeat("b", 40)
	branch := "workflow/onboarding-" + digest[:12]
	valid := PullReadback{Found: true, Merged: true, Mergeable: true, Branch: branch, Head: strings.Repeat("c", 40), Base: "main", BaseHead: baseHead, Body: "Approved Setup Plan SHA-256: " + digest, MergeHead: strings.Repeat("d", 40), State: "closed", ChecksPassed: true, ReviewsClean: true, ContentMatches: true}
	decision, err := DecideOnboardingPull(valid, digest, branch, "main", baseHead)
	if err != nil || decision != PullSatisfied {
		t.Fatalf("valid = %s, %v", decision, err)
	}
	for _, mutate := range []func(*PullReadback){func(v *PullReadback) { v.Head = "" }, func(v *PullReadback) { v.BaseHead = strings.Repeat("e", 40) }, func(v *PullReadback) { v.ContentMatches = false }, func(v *PullReadback) { v.Body = "" }} {
		value := valid
		mutate(&value)
		decision, err = DecideOnboardingPull(value, digest, branch, "main", baseHead)
		if err == nil || decision != PullConflict {
			t.Fatalf("drift = %s, %v", decision, err)
		}
	}
}

func TestDecideOnboardingPullDistinguishesAbsentAndOpenRequired(t *testing.T) {
	digest := strings.Repeat("a", 64)
	baseHead := strings.Repeat("b", 40)
	branch := "workflow/onboarding-" + digest[:12]
	if decision, err := DecideOnboardingPull(PullReadback{}, digest, branch, "main", baseHead); err != nil || decision != PullMissing {
		t.Fatalf("absent = %s,%v", decision, err)
	}
	value := PullReadback{Found: true, Branch: branch, Head: strings.Repeat("c", 40), Base: "main", BaseHead: baseHead, Body: "Approved Setup Plan SHA-256: " + digest, State: "open", Mergeable: true, ChecksPassed: true, ReviewsClean: true, ContentMatches: true}
	if decision, err := DecideOnboardingPull(value, digest, branch, "main", baseHead); err != nil || decision != PullDrift {
		t.Fatalf("open = %s,%v", decision, err)
	}
	value.ChecksPassed = false
	if decision, err := DecideOnboardingPull(value, digest, branch, "main", baseHead); err != nil || decision != PullDrift {
		t.Fatalf("checks pending = %s,%v", decision, err)
	}
}
