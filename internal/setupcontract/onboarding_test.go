package setupcontract

import "testing"

func TestRepositoryOnboardingPlanIsStrictAndCanonical(t *testing.T) {
	raw := []byte(`{"schema_version":1,"plan_id":"onboard-001","kind":"repository_onboarding","target":{"workflow_home":"C:\\Workflow","repository_path":"C:\\repo","github_repository":"owner/repo"},"preconditions":[{"id":"release","kind":"platform_release","subject":"C:\\Workflow","expected":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"id":"head","kind":"git_head","subject":"C:\\repo","expected":""}],"effects":[{"id":"admission","kind":"repository_admission","subject":"owner/repo","action":"verify_and_record","parameters":{"default_branch":"main","manifest_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","contract_version":"1"}}],"expected_results":[{"id":"ready","kind":"repository_admission","subject":"owner/repo","expected":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}`)
	plan, canonical, digest, err := ParsePlan(raw)
	if err != nil || plan.Kind != RepositoryOnboarding || len(canonical) == 0 || len(digest) != 64 {
		t.Fatalf("ParsePlan() = %#v, %q, %q, %v", plan, canonical, digest, err)
	}
	if _, _, _, err := ParsePlan([]byte(`{"unexpected":true,` + string(raw[1:]))); err == nil {
		t.Fatal("accepted unknown request member")
	}
}
