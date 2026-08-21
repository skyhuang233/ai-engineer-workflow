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

func TestWorkflowTagRefRequiresDirectSourceCommit(t *testing.T) {
	source := strings.Repeat("a", 40)
	var valid workflowTagRef
	valid.Object.Type = "commit"
	valid.Object.SHA = source
	if !valid.matchesDirectCommit(source) {
		t.Fatal("rejected a direct tag ref to the manifest source commit")
	}
	valid.Object.Type = "tag"
	if valid.matchesDirectCommit(source) {
		t.Fatal("accepted an annotated tag object")
	}
	valid.Object.Type = "commit"
	valid.Object.SHA = strings.Repeat("b", 40)
	if valid.matchesDirectCommit(source) {
		t.Fatal("accepted a tag ref to another source commit")
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
