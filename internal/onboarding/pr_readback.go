package onboarding

import (
	"errors"
	"strings"
)

type PullReadback struct {
	Found, Merged, Mergeable                             bool
	Number                                               int64
	Branch, Head, Base, BaseHead, Body, MergeHead, State string
	MergedBy, MergedByType                               string
	ChecksPassed, ReviewsClean, ContentMatches           bool
}

type PullDecision string

const (
	PullMissing   PullDecision = "missing"
	PullSatisfied PullDecision = "satisfied"
	PullDrift     PullDecision = "drift"
	PullConflict  PullDecision = "conflict"
)

// DecideOnboardingPull is deliberately pure. Remote callers must provide the
// exact immutable digest-bound evidence; any absent or divergent identity is
// a conflict rather than authority to update or merge a Pull Request.
func DecideOnboardingPull(value PullReadback, digest, branch, base, baseHead, authenticatedLogin string) (PullDecision, error) {
	if !isFullSHA256(digest) || branch != "workflow/onboarding-"+digest[:12] || base == "" || !isGitObjectID(baseHead) {
		return PullConflict, errors.New("approved Onboarding Pull identity is invalid")
	}
	if !value.Found {
		return PullMissing, nil
	}
	if value.Branch != branch || value.Base != base || value.BaseHead != baseHead || !isGitObjectID(value.Head) || value.Body != "Approved Setup Plan SHA-256: "+digest {
		return PullConflict, errors.New("Onboarding Pull Request differs from the approved digest, branch, or base")
	}
	if !value.ContentMatches {
		return PullConflict, errors.New("Onboarding Pull Request managed content differs from the approved digest")
	}
	if value.Merged {
		if !strings.EqualFold(value.State, "closed") || !isGitObjectID(value.MergeHead) {
			return PullConflict, errors.New("merged Onboarding Pull Request lacks exact merge evidence")
		}
		if authenticatedLogin == "" || !strings.EqualFold(value.MergedBy, authenticatedLogin) || !strings.EqualFold(value.MergedByType, "User") {
			return PullConflict, errors.New("Onboarding Pull Request was not merged by the admitted human repository owner")
		}
		return PullSatisfied, nil
	}
	switch strings.ToLower(strings.TrimSpace(value.State)) {
	case "open":
		// This is an exact PR, but it is not yet a merged, admitted result. The
		// caller may wait for checks or use the separately approved merge path.
		if !value.Mergeable || !value.ChecksPassed || !value.ReviewsClean {
			return PullDrift, nil
		}
		return PullDrift, nil
	case "closed":
		// A closed-but-unmerged exact PR may be replaced only by the same digest.
		return PullDrift, nil
	default:
		return PullConflict, errors.New("Onboarding Pull Request has an unknown state")
	}
}

func onboardingPullContentRef(value PullReadback, branch string) string {
	if value.Merged && isGitObjectID(value.Head) {
		return value.Head
	}
	return branch
}

func isGitObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isFullSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
