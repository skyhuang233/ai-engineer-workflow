package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/store"
)

type memoryRemote struct {
	label           Label
	issues, actions bool
	allowed         string
	variable        string
	contentRef      string
	pull            PullReadback
	defaultBranch   RepositoryBranch
	branch          RepositoryBranch
	branchFound     bool
	createdPull     OnboardingPullRequest
	mergeCalls      int
	mergeMethod     string
	repositoryErr   error
	labelErr        error
	featuresErr     error
	variableErr     error
	contractErr     error
	createCalls     int
	labelCalls      int
	featureCalls    int
	variableCalls   int
}

func (m *memoryRemote) Repository(context.Context, string) (RepositoryPolicy, error) {
	if m.repositoryErr != nil {
		return RepositoryPolicy{}, m.repositoryErr
	}
	return RepositoryPolicy{}, ErrRepositoryNotFound
}
func (m *memoryRemote) DefaultBranchHead(context.Context, string) (RepositoryBranch, error) {
	return m.defaultBranch, nil
}
func (m *memoryRemote) OnboardingPull(context.Context, string, string, string, []RequiredCheck) (PullReadback, error) {
	return m.pull, nil
}
func (m *memoryRemote) OnboardingBranch(context.Context, string, string) (RepositoryBranch, bool, error) {
	return m.branch, m.branchFound, nil
}
func (m *memoryRemote) CreateOrUpdateOnboardingPull(_ context.Context, request OnboardingPullRequest) (PullReadback, error) {
	m.createdPull = request
	m.pull = PullReadback{Found: true, Number: 7, State: "open", Branch: request.Branch, Head: request.Head, Base: request.Base, BaseHead: request.BaseHead, Body: "Approved Setup Plan SHA-256: " + request.Digest, Mergeable: true, ChecksPassed: false, ReviewsClean: true, ContentMatches: true}
	return m.pull, nil
}
func (m *memoryRemote) MergeOnboardingPull(_ context.Context, _ string, number int64, expectedHead, method string) (string, error) {
	if number != m.pull.Number || expectedHead != m.pull.Head {
		return "", errors.New("merge identity differs")
	}
	m.mergeCalls++
	m.mergeMethod = method
	mergeHead := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	m.pull.Merged, m.pull.State, m.pull.MergeHead = true, "closed", mergeHead
	m.defaultBranch.Head = mergeHead
	return mergeHead, nil
}
func (m *memoryRemote) VerifyOnboardingContent(_ context.Context, _ string, ref string, _ map[string][]byte) error {
	m.contentRef = ref
	return nil
}
func (m *memoryRemote) CreateRepository(context.Context, string, string, string, bool) error {
	m.createCalls++
	return nil
}

func TestRepositoryCreateOnlyAuthorizesTypedNotFound(t *testing.T) {
	effect := setupcontract.Effect{Kind: "create_repository", Subject: "owner/repo", Parameters: map[string]string{"owner": "owner", "authenticated_login": "owner", "name": "repo", "private": "true"}}
	for _, tc := range []struct {
		name  string
		err   error
		want  setupcontract.EffectStatus
		calls int
	}{{"not found", nil, setupcontract.EffectRequired, 1}, {"auth", errors.New("401"), setupcontract.EffectFailed, 0}, {"forbidden", errors.New("403"), setupcontract.EffectFailed, 0}, {"server", errors.New("500"), setupcontract.EffectFailed, 0}, {"network", context.DeadlineExceeded, setupcontract.EffectFailed, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			r := &memoryRemote{repositoryErr: tc.err}
			a := RepositoryAdapter{Remote: r, PlanDigest: strings.Repeat("a", 64)}
			s, _, err := a.Readback(context.Background(), effect)
			if tc.err != nil && err != tc.err {
				t.Fatalf("err=%v", err)
			}
			if s != tc.want {
				t.Fatalf("status=%s", s)
			}
			if s == setupcontract.EffectRequired {
				if err := a.Apply(context.Background(), effect); err != nil {
					t.Fatal(err)
				}
			}
			if r.createCalls != tc.calls {
				t.Fatalf("calls=%d", r.createCalls)
			}
		})
	}
}

func TestRepositoryAdapterReadbackBindsOnlyExactMergedPull(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mergeHead := "cccccccccccccccccccccccccccccccccccccccc"
	branch := "workflow/onboarding-" + digest[:12]
	remote := &memoryRemote{
		pull:          PullReadback{Found: true, Merged: true, State: "closed", Branch: branch, Head: "dddddddddddddddddddddddddddddddddddddddd", Base: "main", BaseHead: baseHead, Body: "Approved Setup Plan SHA-256: " + digest, MergeHead: mergeHead, ChecksPassed: true, ReviewsClean: true, ContentMatches: true},
		defaultBranch: RepositoryBranch{Name: "main", Head: mergeHead},
	}
	adapter := RepositoryAdapter{Remote: remote, PlanDigest: digest}
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{"base_branch": "main", "base_head": baseHead, "manifest_digest": "manifest", "files_json": `{"AGENTS.md":"bWFuYWdlZAo="}`, "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("exact merged PR = %s, %v", status, err)
	}
	if remote.contentRef != remote.pull.Head {
		t.Fatalf("merged PR content ref = %q, want immutable head %q", remote.contentRef, remote.pull.Head)
	}
	remote.pull.BaseHead = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	status, _, err = adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectConflicting {
		t.Fatalf("base drift = %s, %v", status, err)
	}
}

func TestRepositoryAdapterReadbackRejectsDefaultBaseAdvanceBeforeMerge(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	branch := "workflow/onboarding-" + digest[:12]
	remote := &memoryRemote{
		pull:          PullReadback{Found: true, State: "open", Branch: branch, Head: "cccccccccccccccccccccccccccccccccccccccc", Base: "main", BaseHead: baseHead, Body: "Approved Setup Plan SHA-256: " + digest, Mergeable: true, ChecksPassed: true, ReviewsClean: true, ContentMatches: true},
		defaultBranch: RepositoryBranch{Name: "main", Head: "dddddddddddddddddddddddddddddddddddddddd"},
	}
	adapter := RepositoryAdapter{Remote: remote, PlanDigest: digest}
	effect := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{"base_branch": "main", "base_head": baseHead, "files_json": `{"AGENTS.md":"bWFuYWdlZAo="}`, "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectConflicting {
		t.Fatalf("advanced base = %s, %v", status, err)
	}
}

func TestRepositoryAdapterFastForwardsZeroCommitBaselineFromEffectEvidence(t *testing.T) {
	repository := newRepo(t)
	preMerge := testGitOutput(t, repository, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, "", "clone", "--bare", repository, bare)
	advance := filepath.Join(t.TempDir(), "advance")
	git(t, "", "clone", bare, advance)
	git(t, advance, "config", "user.name", "Test")
	git(t, advance, "config", "user.email", "test@example.com")
	git(t, advance, "commit", "--allow-empty", "-m", "approved onboarding merge")
	mergeHead := testGitOutput(t, advance, "rev-parse", "HEAD")
	git(t, advance, "push", "origin", "main")
	installCapturedGitForPushTest(t, "https://github.com/owner/repo.git", bare)

	remote := &memoryRemote{defaultBranch: RepositoryBranch{Name: "main", Head: mergeHead}}
	adapter := RepositoryAdapter{
		Remote: remote, PlanDigest: strings.Repeat("a", 64),
		MergeHeads:   map[string]string{"repository-contract-pr": mergeHead},
		BaselineHead: map[string]string{"initial-baseline": preMerge},
	}
	effect := setupcontract.Effect{Kind: "local_fast_forward", Subject: repository, Parameters: map[string]string{
		"repository": "owner/repo", "branch": "main", "pre_merge_head": "", "pre_merge_head_effect_id": "initial-baseline", "merge_head_effect_id": "repository-contract-pr",
	}}
	if err := adapter.Apply(context.Background(), effect); err != nil {
		t.Fatalf("zero-commit baseline fast-forward: %v", err)
	}
	if head := testGitOutput(t, repository, "rev-parse", "HEAD"); head != mergeHead {
		t.Fatalf("local HEAD = %s, want approved merge %s", head, mergeHead)
	}
}

func TestRepositoryAdapterReadbackKeepsInitialBaselineSatisfiedAfterFastForward(t *testing.T) {
	repository := newRepo(t)
	baselineHead := testGitOutput(t, repository, "rev-parse", "HEAD")
	files, err := BaselineSnapshot(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "AGENTS.md"), []byte("managed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "add", "AGENTS.md")
	git(t, repository, "commit", "-m", "approved onboarding merge")

	adapter := RepositoryAdapter{Remote: &memoryRemote{}, PlanDigest: strings.Repeat("a", 64)}
	effect := setupcontract.Effect{ID: "initial-baseline", Kind: "initial_baseline", Subject: repository, Parameters: map[string]string{"files_json": string(encoded)}}
	status, _, err := adapter.Readback(context.Background(), effect)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("fast-forwarded Initial Repository Baseline readback = %s, %v", status, err)
	}
	if got := adapter.BaselineHead[effect.ID]; got != baselineHead {
		t.Fatalf("Initial Repository Baseline evidence = %q, want root commit %q", got, baselineHead)
	}
}

func (m *memoryRemote) ReconcileLabel(_ context.Context, _ string, value Label) error {
	m.labelCalls++
	m.label = value
	return nil
}
func (m *memoryRemote) ReconcileFeatures(_ context.Context, _ string, issues, actions bool, allowed string) error {
	m.featureCalls++
	m.issues, m.actions, m.allowed = issues, actions, allowed
	return nil
}
func (m *memoryRemote) VerifyContract(context.Context, string, string, string) error {
	return m.contractErr
}
func (m *memoryRemote) Label(context.Context, string, string) (Label, error) {
	if m.labelErr != nil {
		return Label{}, m.labelErr
	}
	if m.label.Name == "" {
		return Label{}, ErrManagedLabelNotFound
	}
	return m.label, nil
}
func (m *memoryRemote) Features(context.Context, string) (bool, bool, string, error) {
	if m.featuresErr != nil {
		return false, false, "", m.featuresErr
	}
	return m.issues, m.actions, m.allowed, nil
}
func (m *memoryRemote) Variable(context.Context, string, string) (string, error) {
	if m.variableErr != nil {
		return "", m.variableErr
	}
	if m.variable == "" {
		return "", ErrRepositoryVariableNotFound
	}
	return m.variable, nil
}
func (m *memoryRemote) ReconcileVariable(_ context.Context, _ string, _ string, value string) error {
	m.variableCalls++
	m.variable = value
	return nil
}

func TestRepositoryAdapterReconcilesManagedGitHubResourcesIdempotently(t *testing.T) {
	remote := &memoryRemote{}
	adapter := RepositoryAdapter{Remote: remote, PlanDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, effect := range []setupcontract.Effect{
		{Kind: "github_label", Subject: "owner/repo#workflow", Parameters: map[string]string{"name": "workflow", "color": "123456", "description": "managed"}},
		{Kind: "repository_features", Subject: "owner/repo", Parameters: map[string]string{"issues": "true", "actions": "true", "allowed_actions": "selected"}},
		{Kind: "repository_variable", Subject: "owner/repo", Parameters: map[string]string{"name": "WORKFLOW", "value": "enabled"}},
	} {
		if err := adapter.Apply(context.Background(), effect); err != nil {
			t.Fatal(err)
		}
		status, _, err := adapter.Readback(context.Background(), effect)
		if err != nil || status != setupcontract.EffectSatisfied {
			t.Fatalf("%s = %s, %v", effect.Kind, status, err)
		}
	}
	remote.label.Color = "ffffff"
	status, _, err := adapter.Readback(context.Background(), setupcontract.Effect{Kind: "github_label", Subject: "owner/repo#workflow", Parameters: map[string]string{"name": "workflow", "color": "123456", "description": "managed"}})
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("drift = %s, %v", status, err)
	}
}

func TestManagedResourceReadbackOnlyReconcilesTypedNotFound(t *testing.T) {
	digest := strings.Repeat("a", 64)
	cases := []struct {
		name      string
		effect    setupcontract.Effect
		missing   error
		setError  func(*memoryRemote, error)
		callCount func(*memoryRemote) int
	}{
		{
			name: "label", effect: setupcontract.Effect{Kind: "github_label", Subject: "owner/repo#workflow", Parameters: map[string]string{"name": "workflow", "color": "123456", "description": "managed"}}, missing: ErrManagedLabelNotFound,
			setError: func(r *memoryRemote, err error) { r.labelErr = err }, callCount: func(r *memoryRemote) int { return r.labelCalls },
		},
		{
			name: "features", effect: setupcontract.Effect{Kind: "repository_features", Subject: "owner/repo", Parameters: map[string]string{"issues": "true", "actions": "true", "allowed_actions": "selected"}}, missing: ErrRepositoryNotFound,
			setError: func(r *memoryRemote, err error) { r.featuresErr = err }, callCount: func(r *memoryRemote) int { return r.featureCalls },
		},
		{
			name: "variable", effect: setupcontract.Effect{Kind: "repository_variable", Subject: "owner/repo", Parameters: map[string]string{"name": "WORKFLOW", "value": "enabled"}}, missing: ErrRepositoryVariableNotFound,
			setError: func(r *memoryRemote, err error) { r.variableErr = err }, callCount: func(r *memoryRemote) int { return r.variableCalls },
		},
	}
	for _, resource := range cases {
		t.Run(resource.name, func(t *testing.T) {
			for _, failure := range []struct {
				name        string
				err         error
				shouldApply bool
			}{
				{name: "typed not found", err: resource.missing, shouldApply: true},
				{name: "auth", err: errors.New("401 unauthorized")},
				{name: "forbidden", err: errors.New("403 forbidden")},
				{name: "server", err: errors.New("500 server")},
				{name: "network", err: context.DeadlineExceeded},
			} {
				t.Run(failure.name, func(t *testing.T) {
					remote := &memoryRemote{}
					resource.setError(remote, failure.err)
					adapter := RepositoryAdapter{Remote: remote, PlanDigest: digest}
					status, _, err := adapter.Readback(context.Background(), resource.effect)
					if failure.shouldApply {
						if err != nil || status != setupcontract.EffectRequired {
							t.Fatalf("readback=%s,%v", status, err)
						}
						if err := adapter.Apply(context.Background(), resource.effect); err != nil {
							t.Fatal(err)
						}
						if resource.callCount(remote) != 1 {
							t.Fatalf("typed missing did not reconcile exactly once: %d", resource.callCount(remote))
						}
						return
					}
					if status != setupcontract.EffectFailed || !errors.Is(err, failure.err) {
						t.Fatalf("readback=%s,%v", status, err)
					}
					if resource.callCount(remote) != 0 {
						t.Fatalf("read error authorized mutation: %d", resource.callCount(remote))
					}
				})
			}
		})
	}
}

func TestRepositoryAdmissionRecordsEligibilityOnlyAfterLiveContractVerification(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	effect := setupcontract.Effect{Kind: "repository_admission", Subject: "owner/repo", Parameters: map[string]string{"default_branch": "main", "manifest_digest": strings.Repeat("b", 64), "contract_version": "1"}}
	remote := &memoryRemote{contractErr: errors.New("remote contract unavailable")}
	repositoryPath := filepath.Join(t.TempDir(), "repo")
	adapter := RepositoryAdapter{Remote: remote, Store: database, PlanDigest: strings.Repeat("a", 64), RepositoryPath: repositoryPath}
	if err := adapter.Apply(context.Background(), effect); err == nil {
		t.Fatal("admission was recorded without a live contract")
	}
	if _, err := database.RepositoryAdmission(context.Background(), effect.Subject); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed verification left an admission record: %v", err)
	}
	remote.contractErr = nil
	if err := adapter.Apply(context.Background(), effect); err != nil {
		t.Fatal(err)
	}
	record, err := database.RepositoryAdmission(context.Background(), effect.Subject)
	if err != nil || !record.Eligible {
		t.Fatalf("live verification did not record eligible admission: %#v,%v", record, err)
	}
	runtime, err := database.RepositoryRuntimeConfiguration(context.Background(), effect.Subject)
	if err != nil {
		t.Fatalf("clean Repository Admission did not seed runtime configuration: %v", err)
	}
	if runtime.Repository != effect.Subject || runtime.DefaultBranch != "main" || runtime.SourcePath != repositoryPath || runtime.RootIssueNumber != 0 {
		t.Fatalf("runtime configuration seed = %#v", runtime)
	}
	t.Logf("Repository Admitted after live verification; initial runtime seed repository=%s branch=%s source=%q root=%d", runtime.Repository, runtime.DefaultBranch, runtime.SourcePath, runtime.RootIssueNumber)
}

func TestRepositoryAdmissionReadbackRetriesMissingRuntimeSeed(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	manifestDigest := strings.Repeat("b", 64)
	if err := database.RecordRepositoryAdmission(ctx, store.RepositoryAdmission{Repository: "owner/repo", OnboardingPlanDigestSHA256: digest, ContractVersion: "1", ManifestDigestSHA256: manifestDigest, Eligible: true, VerifiedAt: now}); err != nil {
		t.Fatal(err)
	}
	effect := setupcontract.Effect{Kind: "repository_admission", Subject: "owner/repo", Parameters: map[string]string{"default_branch": "main", "manifest_digest": manifestDigest, "contract_version": "1"}}
	adapter := RepositoryAdapter{Remote: &memoryRemote{}, Store: database, PlanDigest: digest, RepositoryPath: filepath.Join(t.TempDir(), "repo")}
	status, _, err := adapter.Readback(ctx, effect)
	if err != nil || status != setupcontract.EffectRequired {
		t.Fatalf("partial admission readback = %s, %v", status, err)
	}
	if err := adapter.Apply(ctx, effect); err != nil {
		t.Fatalf("retry partial admission: %v", err)
	}
	if _, err := database.RepositoryRuntimeConfiguration(ctx, effect.Subject); err != nil {
		t.Fatalf("retry did not repair runtime seed: %v", err)
	}
}

type fakeBranchWriter struct {
	prepared  PreparedOnboardingBranch
	published bool
	request   OnboardingPullRequest
}

func (w *fakeBranchWriter) Prepare(_ context.Context, request OnboardingPullRequest, _ string, _ GitCredential) (PreparedOnboardingBranch, error) {
	w.request = request
	return w.prepared, nil
}
func (w *fakeBranchWriter) Publish(context.Context, PreparedOnboardingBranch, string, string, GitCredential) error {
	w.published = true
	return nil
}

func TestRepositoryAdapterCreatesExactUnmergedOnboardingPull(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	head := "cccccccccccccccccccccccccccccccccccccccc"
	remote := &memoryRemote{defaultBranch: RepositoryBranch{Name: "main", Head: baseHead}}
	writer := &fakeBranchWriter{prepared: PreparedOnboardingBranch{Branch: "workflow/onboarding-" + digest[:12], Head: head}}
	adapter := RepositoryAdapter{Remote: remote, Owner: "owner", Credential: GitCredential{Token: "pat"}, PlanDigest: digest, BranchWriter: writer}
	effect := setupcontract.Effect{Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{
		"base_branch": "main", "base_head": baseHead, "source_url": "https://github.com/owner/repo.git",
		"files_json": `{"AGENTS.md":"bWFuYWdlZAo="}`, "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`,
	}}
	if err := adapter.Apply(context.Background(), effect); err != nil {
		t.Fatal(err)
	}
	if !writer.published || string(writer.request.Files["AGENTS.md"]) != "managed\n" || remote.createdPull.Head != head || remote.createdPull.Digest != digest {
		t.Fatalf("onboarding pull = %#v; branch writer = %#v", remote.createdPull, writer)
	}
}

func TestRepositoryAdapterReusesOnlyExactOpenOnboardingPull(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	head := "cccccccccccccccccccccccccccccccccccccccc"
	branch := "workflow/onboarding-" + digest[:12]
	remote := &memoryRemote{
		defaultBranch: RepositoryBranch{Name: "main", Head: baseHead},
		pull:          PullReadback{Found: true, Number: 7, State: "open", Branch: branch, Head: head, Base: "main", BaseHead: baseHead, Body: "Approved Setup Plan SHA-256: " + digest, Mergeable: true, ChecksPassed: false, ReviewsClean: true, ContentMatches: true},
	}
	writer := &fakeBranchWriter{prepared: PreparedOnboardingBranch{Branch: branch, Head: head}}
	adapter := RepositoryAdapter{Remote: remote, Owner: "owner", Credential: GitCredential{Token: "pat"}, PlanDigest: digest, BranchWriter: writer}
	effect := setupcontract.Effect{Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{"base_branch": "main", "base_head": baseHead, "source_url": "https://github.com/owner/repo.git", "files_json": `{"AGENTS.md":"bWFuYWdlZAo="}`, "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}}
	if err := adapter.Apply(context.Background(), effect); err != nil {
		t.Fatal(err)
	}
	if writer.published || remote.createdPull.Repository != "" {
		t.Fatalf("exact existing PR was mutated: published=%v request=%#v", writer.published, remote.createdPull)
	}
}

func TestRepositoryAdapterRejectsPullHeadDriftWithoutMutation(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	branch := "workflow/onboarding-" + digest[:12]
	remote := &memoryRemote{
		defaultBranch: RepositoryBranch{Name: "main", Head: baseHead},
		pull:          PullReadback{Found: true, Number: 7, State: "open", Branch: branch, Head: "dddddddddddddddddddddddddddddddddddddddd", Base: "main", BaseHead: baseHead, Body: "Approved Setup Plan SHA-256: " + digest, Mergeable: true, ChecksPassed: true, ReviewsClean: true, ContentMatches: true},
	}
	writer := &fakeBranchWriter{prepared: PreparedOnboardingBranch{Branch: branch, Head: "cccccccccccccccccccccccccccccccccccccccc"}}
	adapter := RepositoryAdapter{Remote: remote, Owner: "owner", Credential: GitCredential{Token: "pat"}, PlanDigest: digest, BranchWriter: writer}
	effect := setupcontract.Effect{Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{"base_branch": "main", "base_head": baseHead, "source_url": "https://github.com/owner/repo.git", "files_json": `{"AGENTS.md":"bWFuYWdlZAo="}`, "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`}}
	if err := adapter.Apply(context.Background(), effect); err == nil {
		t.Fatal("expected deterministic PR head drift rejection")
	}
	if writer.published || remote.createdPull.Repository != "" {
		t.Fatalf("drifted PR was mutated: published=%v request=%#v", writer.published, remote.createdPull)
	}
}

func TestRepositoryAdapterMergesExactPullThenRecordsAndVerifiesAdmission(t *testing.T) {
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	manifest := strings.Repeat("f", 64)
	baseHead := strings.Repeat("b", 40)
	head := strings.Repeat("c", 40)
	branch := "workflow/onboarding-" + digest[:12]
	remote := &memoryRemote{
		defaultBranch: RepositoryBranch{Name: "main", Head: baseHead},
		pull:          PullReadback{Found: true, Number: 7, State: "open", Branch: branch, Head: head, Base: "main", BaseHead: baseHead, Body: "Approved Setup Plan SHA-256: " + digest, Mergeable: true, ChecksPassed: true, ReviewsClean: true, ContentMatches: true},
	}
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "generation-local-workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	writer := &fakeBranchWriter{prepared: PreparedOnboardingBranch{Branch: branch, Head: head}}
	adapter := RepositoryAdapter{Remote: remote, Owner: "owner", Credential: GitCredential{Token: "pat"}, PlanDigest: digest, Store: database, BranchWriter: writer}
	contract := setupcontract.Effect{ID: "repository-contract-pr", Kind: "repository_contract_pr", Subject: "owner/repo", Parameters: map[string]string{"base_branch": "main", "base_head": baseHead, "source_url": "https://github.com/owner/repo.git", "files_json": `{"AGENTS.md":"bWFuYWdlZAo="}`, "required_checks_json": `[{"context":"workflow-contract","app_id":15368}]`, "manifest_digest": manifest, "merge_method": "squash"}}
	if err := adapter.Apply(ctx, contract); err != nil {
		t.Fatal(err)
	}
	if remote.mergeCalls != 1 || writer.published || remote.createdPull.Repository != "" {
		t.Fatalf("merge authority was not exact: calls=%d published=%v create=%#v", remote.mergeCalls, writer.published, remote.createdPull)
	}
	if remote.mergeMethod != "squash" {
		t.Fatalf("merge method=%q", remote.mergeMethod)
	}
	status, _, err := adapter.Readback(ctx, contract)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("merged pull readback = %s, %v", status, err)
	}
	if err := adapter.Apply(ctx, contract); err != nil {
		t.Fatal(err)
	}
	if remote.mergeCalls != 1 {
		t.Fatalf("already merged exact PR was merged again: %d", remote.mergeCalls)
	}
	admission := setupcontract.Effect{Kind: "repository_admission", Subject: "owner/repo", Parameters: map[string]string{"default_branch": "main", "manifest_digest": manifest, "contract_version": "1"}}
	if err := adapter.Apply(ctx, admission); err != nil {
		t.Fatal(err)
	}
	record, err := database.RepositoryAdmission(ctx, "owner/repo")
	if err != nil || record.OnboardingPlanDigestSHA256 != digest || record.ManifestDigestSHA256 != manifest || !record.Eligible {
		t.Fatalf("generation-local admission = %#v, %v", record, err)
	}
	status, _, err = adapter.Readback(ctx, admission)
	if err != nil || status != setupcontract.EffectSatisfied {
		t.Fatalf("admission final verify = %s, %v", status, err)
	}
}
