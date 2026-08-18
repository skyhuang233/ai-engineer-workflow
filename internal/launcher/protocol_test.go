package launcher

import (
	"strings"
	"testing"
)

func TestDecodeRequestRejectsUnknownFieldsAndAcceptsBOM(t *testing.T) {
	request, err := DecodeRequest([]byte("\xef\xbb\xbf{\"schema_version\":1,\"operation\":\"verify\",\"workflow_home\":\"C:\\\\workflow\"}"))
	if err != nil || request.Operation != Verify {
		t.Fatalf("DecodeRequest() = %#v, %v", request, err)
	}
	if _, err := DecodeRequest([]byte(`{"schema_version":1,"operation":"verify","workflow_home":"C:\\workflow","surprise":true}`)); err == nil {
		t.Fatal("accepted an unknown request field")
	}
}

func TestDecodeRequestRequiresTargetInspectPurpose(t *testing.T) {
	_, err := DecodeRequest([]byte(`{"schema_version":1,"operation":"inspect","workflow_home":"C:\\workflow"}`))
	if err == nil || !strings.Contains(err.Error(), "purpose") {
		t.Fatalf("error = %v, want purpose validation", err)
	}
}

func TestDecodeRequestCarriesGitHubOwnerOnlyForTargetOperations(t *testing.T) {
	request, err := DecodeRequest([]byte(`{"schema_version":1,"operation":"inspect","purpose":"target_state","workflow_home":"C:\\workflow","target_version":"0.0.1","bundle_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","github_owner":"owner"}`))
	if err != nil || request.GitHubOwner != "owner" {
		t.Fatalf("target owner request=%#v, err=%v", request, err)
	}
	if _, err := DecodeRequest([]byte(`{"schema_version":1,"operation":"verify","workflow_home":"C:\\workflow","github_owner":"owner"}`)); err == nil {
		t.Fatal("verify accepted an owner-bearing target field")
	}
}

func TestDecodeRequestRejectsNonCanonicalBundleDigest(t *testing.T) {
	for _, digest := range []string{strings.Repeat("a", 64), "sha256:" + strings.Repeat("g", 64), "sha256:" + strings.Repeat("A", 64)} {
		request := `{"schema_version":1,"operation":"inspect","purpose":"target_state","workflow_home":"C:\\workflow","target_version":"0.0.3","bundle_digest":"` + digest + `","github_owner":"owner"}`
		if _, err := DecodeRequest([]byte(request)); err == nil {
			t.Fatalf("accepted non-canonical digest %q", digest)
		}
	}
}
