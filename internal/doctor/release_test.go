package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

func TestExactWorkflowAssetsRequiresTheAtomicSet(t *testing.T) {
	valid := []releaseAsset{
		{ID: 1, Name: workflowrelease.BundleAssetName, Digest: "sha256:" + strings.Repeat("a", 64)},
		{ID: 2, Name: workflowrelease.ManifestAssetName, Digest: "sha256:" + strings.Repeat("b", 64)},
		{ID: 3, Name: workflowrelease.SBOMAssetName, Digest: "sha256:" + strings.Repeat("c", 64)},
	}
	if _, err := exactWorkflowAssets(valid); err != nil {
		t.Fatalf("valid assets: %v", err)
	}
	for name, mutate := range map[string]func([]releaseAsset) []releaseAsset{
		"missing":   func(v []releaseAsset) []releaseAsset { return v[:2] },
		"duplicate": func(v []releaseAsset) []releaseAsset { v[2].Name = v[1].Name; return v },
		"extra":     func(v []releaseAsset) []releaseAsset { return append(v, releaseAsset{ID: 4, Name: "extra"}) },
	} {
		t.Run(name, func(t *testing.T) {
			input := append([]releaseAsset(nil), valid...)
			if _, err := exactWorkflowAssets(mutate(input)); err == nil {
				t.Fatal("accepted a non-atomic asset set")
			}
		})
	}
}

func TestWorkerSBOMRequiresNamedSPDX23(t *testing.T) {
	if err := validateWorkerSBOM([]byte(`{"spdxVersion":"SPDX-2.3","name":"workflow-worker"}`)); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkerSBOM([]byte(`{"spdxVersion":"SPDX-2.2","name":"workflow-worker"}`)); err == nil {
		t.Fatal("accepted a non-SPDX-2.3 document")
	}
}

func TestReleaseFetcherRejectsAnUnboundRepositoryBeforeNetwork(t *testing.T) {
	config := validConfig()
	if _, _, err := (ReleaseFetcher{WorkflowRepository: "other/repo"}).Fetch(context.Background(), config, "token"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPublisherTagMessageRequiresExactRunAndAttempt(t *testing.T) {
	valid := publisherTagMessagePattern.FindStringSubmatch("Workflow publisher provenance\nrun_id=456\nrun_attempt=2")
	if len(valid) != 3 || valid[1] != "456" || valid[2] != "2" {
		t.Fatal("rejected exact publisher provenance")
	}
	for _, invalid := range []string{"run_id=456", "Workflow publisher provenance\nrun_id=0\nrun_attempt=2", "Workflow publisher provenance\nrun_id=456\nrun_attempt=0\n"} {
		if publisherTagMessagePattern.MatchString(invalid) {
			t.Fatalf("accepted invalid publisher provenance %q", invalid)
		}
	}
}

func TestReleaseIntegrationRequiresTwoParentsIncludingPullHead(t *testing.T) {
	head := strings.Repeat("b", 40)
	commit := releaseIntegrationCommit{Parents: []releaseCommitParent{{SHA: strings.Repeat("a", 40)}, {SHA: head}}}
	if !commit.containsExactPullHead(head) {
		t.Fatal("rejected valid no-ff integration commit")
	}
	commit.Parents = commit.Parents[:1]
	if commit.containsExactPullHead(head) {
		t.Fatal("accepted one-parent squash or rebase result")
	}
	commit.Parents = append(commit.Parents, releaseCommitParent{SHA: strings.Repeat("c", 40)})
	if commit.containsExactPullHead(head) {
		t.Fatal("accepted merge that omitted the exact pull request head")
	}
}

func TestQualificationMustCompleteNoLaterThanMerge(t *testing.T) {
	if !completedNoLaterThan("2026-08-21T01:00:00Z", "2026-08-21T02:00:00Z") {
		t.Fatal("rejected qualification completed before merge")
	}
	if completedNoLaterThan("2026-08-21T03:00:00Z", "2026-08-21T02:00:00Z") {
		t.Fatal("accepted post-merge qualification")
	}
	if completedNoLaterThan("invalid", "2026-08-21T02:00:00Z") {
		t.Fatal("accepted invalid qualification completion time")
	}
}
