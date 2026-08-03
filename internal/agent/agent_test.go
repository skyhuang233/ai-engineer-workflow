package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/agent"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

type fakeRuntime struct {
	results          []worker.Result
	specs            []worker.Spec
	err              error
	dirty            bool
	failAfterCommit  bool
	switchBranch     bool
	ignoredFile      bool
	deliveryOutput   []byte
	deliveryDeadline time.Time
}

type blockingFailureRuntime struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (r blockingFailureRuntime) Run(_ context.Context, _ worker.Spec) (worker.Result, error) {
	r.started <- struct{}{}
	<-r.release
	return worker.Result{Output: []byte(`{"type":"thread.started","thread_id":"expired-agent"}`), ContainerID: "expired-container"}, errors.New("worker crashed")
}

func (r *fakeRuntime) Run(ctx context.Context, spec worker.Spec) (worker.Result, error) {
	r.specs = append(r.specs, spec)
	if spec.Command[0] == "no-mistakes" {
		output := []byte("run:\n  id: delivery-1\n  status: completed\noutcome: passed\n")
		if len(r.deliveryOutput) > 0 {
			output = r.deliveryOutput
		}
		r.deliveryDeadline, _ = ctx.Deadline()
		return worker.Result{Output: output, Stdout: output, ContainerID: "delivery-container"}, nil
	}
	marker := "initial"
	if len(spec.Command) > 2 && spec.Command[2] == "resume" {
		marker = "resume"
	}
	if err := os.WriteFile(filepath.Join(spec.WorkspacePath, "agent.txt"), []byte("candidate "+marker+"\n"), 0o644); err != nil {
		return worker.Result{}, err
	}
	if r.dirty {
		if r.ignoredFile {
			if err := os.MkdirAll(filepath.Join(spec.WorkspacePath, "ignored"), 0o755); err != nil {
				return worker.Result{}, err
			}
			if err := os.WriteFile(filepath.Join(spec.WorkspacePath, "ignored", "evidence.log"), []byte("ignored evidence\n"), 0o644); err != nil {
				return worker.Result{}, err
			}
		}
		result := r.results[0]
		r.results = r.results[1:]
		return result, r.err
	}
	for _, args := range [][]string{{"add", "agent.txt"}, {"commit", "-m", "candidate"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = spec.WorkspacePath
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Agent", "GIT_AUTHOR_EMAIL=agent@example.com", "GIT_COMMITTER_NAME=Agent", "GIT_COMMITTER_EMAIL=agent@example.com")
		if output, err := cmd.CombinedOutput(); err != nil {
			return worker.Result{}, err
		} else if len(output) > 0 {
			_ = output
		}
	}
	if r.switchBranch {
		cmd := exec.Command("git", "switch", "-c", "unexpected")
		cmd.Dir = spec.WorkspacePath
		if output, err := cmd.CombinedOutput(); err != nil {
			return worker.Result{}, fmt.Errorf("switch branch: %w (%s)", err, output)
		}
	}
	result := r.results[0]
	r.results = r.results[1:]
	if r.failAfterCommit {
		return result, r.err
	}
	return result, nil
}

func TestControllerSnapshotsAndRestoresAnAbnormalWorkerRun(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	runtime := &fakeRuntime{dirty: true, ignoredFile: true, err: errors.New("worker crashed"), results: []worker.Result{{Output: []byte(`{"type":"thread.started","thread_id":"codex-failed"}` + "\n" + `{"type":"result","summary":"partial"}`), ContainerID: "container-failed"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement the ticket")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	session, err := db.TicketSession(ctx, version.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if session.CodexSessionID != "codex-failed" {
		t.Fatalf("failed-run Codex session = %q, want codex-failed", session.CodexSessionID)
	}
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = session.WorkspacePath
	if output, err := status.CombinedOutput(); err != nil || string(output) != "" {
		t.Fatalf("restored workspace status = %q, err = %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(session.WorkspacePath, "agent.txt")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted worker file still exists, err = %v", err)
	}
	diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(diagnostic); err != nil {
		t.Fatalf("diagnostic snapshot %q: %v", diagnostic, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(diagnostic), "residue", "agent.txt")); err != nil {
		t.Fatalf("uncommitted residue evidence: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(diagnostic), "ignored", "ignored", "evidence.log")); err != nil {
		t.Fatalf("ignored residue evidence: %v", err)
	}
	if _, err := os.Stat(filepath.Join(session.WorkspacePath, "ignored", "evidence.log")); !os.IsNotExist(err) {
		t.Fatalf("ignored worker residue still exists, err = %v", err)
	}
	audit, err := db.WorkerAudit(ctx, claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.ContainerID != "container-failed" || audit.GitHubWriteCredentials || !strings.Contains(audit.ExtraHostsJSON, worker.GatewayHostMapping) {
		t.Fatalf("audit = %#v", audit)
	}
	next, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if next.SessionID != claim.SessionID || next.Attempt != claim.Attempt+1 || next.LeaseGeneration != claim.LeaseGeneration+1 {
		t.Fatalf("replacement claim = %#v, want same session and next run", next)
	}
}

func TestControllerDoesNotRestoreWorkspaceAfterConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	started := make(chan struct{})
	release := make(chan struct{})
	expired := agent.Controller{Store: db, Workspace: manager, Runtime: blockingFailureRuntime{started: started, release: release}, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
	errCh := make(chan error, 1)
	go func() {
		_, err := expired.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement"))
		errCh <- err
	}()
	<-started

	replacement, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: claim.TicketID, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: claim.LeaseExpiresAt.Add(time.Second)})
	if err != nil {
		t.Fatalf("claim replacement: %v", err)
	}
	replacementRuntime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("replacement-session", "replacement"), ContainerID: "replacement-container"}}}
	replacementController := agent.Controller{Store: db, Workspace: manager, Runtime: replacementRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
	candidate, err := replacementController.Run(ctx, candidateRequest(replacement, source, "ticket-1", "replace"))
	if err != nil {
		t.Fatalf("run replacement: %v", err)
	}
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(session.WorkspacePath, "replacement.txt")
	if err := os.WriteFile(marker, []byte("replacement work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errCh; err == nil {
		t.Fatal("expired worker run returned nil error")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expired worker removed replacement work: %v", err)
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = session.WorkspacePath
	output, err := head.Output()
	if err != nil || strings.TrimSpace(string(output)) != candidate.Commit {
		t.Fatalf("workspace HEAD = %q, err = %v, want replacement %q", output, err, candidate.Commit)
	}
	if _, err := db.RunDiagnostic(ctx, claim.RunID); err != nil {
		t.Fatalf("expired worker did not retain diagnostics: %v", err)
	}
}

func createClaim(t *testing.T, ctx context.Context, root string) (*store.Store, store.PlanVersion, store.TicketClaim) {
	t.Helper()
	db, err := store.Open(ctx, filepath.Join(root, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := plan.Snapshot{Repository: "owner/repo", Root: plan.Issue{ID: 100, Number: 10, Labels: []string{plan.PlanLabel}}, Children: []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}}}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-owner", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return db, version, claim
}

func TestControllerDelegatesDeliveryCycleToNoMistakes(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot := plan.Snapshot{
		Repository: "owner/repo",
		Root:       plan.Issue{ID: 100, Number: 10, Body: "human specification", Labels: []string{plan.PlanLabel}},
		Children:   []plan.Issue{{ID: 1, Number: 11, Title: "first", Labels: []string{plan.TicketLabel}, State: "open"}},
	}
	fingerprint, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	version, err := db.BeginActivation(ctx, snapshot, fingerprint, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkActive(ctx, version.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: 1, Owner: "agent-owner", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	first := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session-1", "implemented"), ContainerID: "container-1"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: first, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0", "git": "2.0.0"}}
	candidate, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement the ticket"))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.CodexSessionID != "codex-session-1" || candidate.Commit == "" {
		t.Fatalf("candidate = %#v", candidate)
	}
	recovered, err := db.CandidateRevision(ctx, candidate.RunID)
	if err != nil || recovered.CommitSHA != candidate.Commit {
		t.Fatalf("recovered candidate = %#v, err=%v", recovered, err)
	}
	if keys, err := db.DueDeliveryOutboxKeys(ctx, time.Now().UTC(), 8); err != nil || len(keys) != 0 {
		t.Fatalf("candidate acceptance queued delivery commands: keys=%#v, err=%v", keys, err)
	}
	if len(first.specs) != 2 || strings.Join(first.specs[1].Command, " ") != "no-mistakes axi run --intent implement the ticket" {
		t.Fatalf("first worker spec = %#v", first.specs)
	}
	if command := first.specs[0].Command; len(command) != 7 || command[0] != "codex" || command[1] != "exec" || command[2] != "--json" || command[3] != "--output-schema" || command[5] != "--skip-git-repo-check" || command[6] != "implement the ticket" {
		t.Fatalf("Codex command = %#v", command)
	} else if schema, err := os.ReadFile(command[4]); err != nil || !strings.Contains(string(schema), `"summary"`) {
		t.Fatalf("Candidate output schema = %q, err = %v", schema, err)
	}
	if first.specs[0].AgentIdentity == "" || len(first.specs[0].Mounts) != 2 || first.specs[0].Environment["GITHUB_TOKEN"] != "" {
		t.Fatalf("first worker isolation = %#v", first.specs[0])
	}
	if first.specs[0].Environment["NO_MISTAKES_RUN_ID"] != "" {
		t.Fatalf("Codex worker received Delivery Controller environment = %#v", first.specs[0].Environment)
	}
	if first.specs[1].Environment["NO_MISTAKES_RUN_ID"] == claim.RunID || first.specs[1].Environment["NO_MISTAKES_LEASE_TOKEN"] == claim.LeaseToken || first.specs[1].Environment["NO_MISTAKES_LEASE_GENERATION"] != fmt.Sprint(claim.LeaseGeneration+1) || first.specs[1].Environment["NO_MISTAKES_REPOSITORY"] != "owner/repo" || first.specs[1].Environment["NO_MISTAKES_BRANCH"] != "ticket-1" || first.specs[1].Environment["NO_MISTAKES_COMMIT_SHA"] != candidate.Commit {
		t.Fatalf("Delivery Controller Gateway fence environment = %#v", first.specs[1].Environment)
	}
	if !first.deliveryDeadline.After(claim.LeaseExpiresAt) {
		t.Fatalf("Delivery Controller deadline = %s, want after Agent deadline %s", first.deliveryDeadline, claim.LeaseExpiresAt)
	}

	if _, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "replacement-owner", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second)}); !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("claim while delivery handoff is pending = %v, want fencing conflict", err)
	}
	revision, err := db.ClaimReviewRevision(ctx, version.ID, claim.TicketID, time.Minute, time.Now().UTC().Add(time.Second), 1)
	if err != nil {
		t.Fatalf("claim review revision: %v", err)
	}
	if revision.SessionID != claim.SessionID || revision.Attempt != claim.Attempt+1 || revision.LeaseGeneration != claim.LeaseGeneration+2 {
		t.Fatalf("review revision claim = %#v", revision)
	}
	second := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session-1", "revised"), ContainerID: "container-2"}}}
	controller.Runtime = second
	if _, err := controller.Run(ctx, candidateRequest(revision, source, "ticket-1", "address the review feedback")); err != nil {
		t.Fatalf("run review revision: %v", err)
	}
	if len(second.specs) != 2 || strings.Join(second.specs[1].Command, " ") != "no-mistakes axi run --intent address the review feedback" {
		t.Fatalf("review worker specs = %#v", second.specs)
	}
	if command := second.specs[0].Command; len(command) != 9 || command[0] != "codex" || command[1] != "exec" || command[2] != "resume" || command[3] != "--json" || command[4] != "--output-schema" || command[6] != "--skip-git-repo-check" || command[7] != "codex-session-1" || command[8] != "address the review feedback" {
		t.Fatalf("resumed Codex command = %#v", command)
	}
	if !json.Valid(candidate.StructuredOutput) {
		t.Fatalf("structured output is not JSON: %s", candidate.StructuredOutput)
	}
}

func TestControllerMarksFailedDeliveryNeedsAttention(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}}, deliveryOutput: []byte("run:\n  status: completed\noutcome: failed\n")}
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Fatalf("failed Delivery Controller error = %v", err)
	}
	if _, err := db.CandidateRevision(ctx, claim.RunID); err != nil {
		t.Fatalf("Candidate acceptance was not durable: %v", err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 1 || !strings.Contains(questions[0].Prompt, "Delivery Controller failed") {
		t.Fatalf("Delivery Controller recovery question = %#v, err = %v", questions, err)
	}
	if _, err := db.ClaimReviewRevision(ctx, version.ID, claim.TicketID, time.Minute, time.Now().UTC(), 1); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("review claim after failed delivery = %v, want not ready", err)
	}
}

func TestControllerPreservesCommittedFailureAndRejectsBranchChanges(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		runtime *fakeRuntime
	}{
		{name: "committed failure", runtime: &fakeRuntime{failAfterCommit: true, err: errors.New("worker crashed after commit"), results: []worker.Result{{Output: codexOutput("codex-failed", "partial"), ContainerID: "container-failed"}}}},
		{name: "branch change", runtime: &fakeRuntime{switchBranch: true, results: []worker.Result{{Output: codexOutput("codex-failed", "partial"), ContainerID: "container-failed"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initRepository(t)
			root := t.TempDir()
			db, _, claim := createClaim(t, ctx, root)
			defer db.Close()
			manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
			controller := agent.Controller{Store: db, Workspace: manager, Runtime: test.runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
			if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil {
				t.Fatal("abnormal worker run returned nil error")
			}
			diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
			if err != nil {
				t.Fatal(err)
			}
			patch, err := os.ReadFile(filepath.Join(filepath.Dir(diagnostic), "revision.patch"))
			if err != nil || !strings.Contains(string(patch), "candidate initial") {
				t.Fatalf("revision evidence = %q, err = %v", patch, err)
			}
			session, err := db.TicketSession(ctx, claim.VersionID, claim.TicketID)
			if err != nil {
				t.Fatal(err)
			}
			branch := exec.Command("git", "branch", "--show-current")
			branch.Dir = session.WorkspacePath
			if output, err := branch.Output(); err != nil || strings.TrimSpace(string(output)) != "ticket-1" {
				t.Fatalf("restored branch = %q, err = %v", output, err)
			}
		})
	}
}

func TestControllerRejectsCredentialBearingWorkspaceSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"},
	}
	_, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: "https://user:token@github.com/owner/repo.git", Branch: "ticket-1", Prompt: "implement"})
	if err == nil || !strings.Contains(err.Error(), "absolute local path") {
		t.Fatalf("credential-bearing source error = %v", err)
	}
	if len(runtime.specs) != 0 {
		t.Fatal("worker started with a credential-bearing workspace source")
	}
	if err := db.ReserveWorkerLaunch(ctx, claim, time.Now().UTC()); err != nil {
		t.Fatalf("preflight failure reserved worker launch: %v", err)
	}
}

func TestControllerRejectsPersistedExternalWorkspaceRemote(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, firstClaim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	firstRuntime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "first"), ContainerID: "container-1"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: firstRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
	if _, err := controller.Run(ctx, candidateRequest(firstClaim, source, "ticket-1", "implement")); err != nil {
		t.Fatal(err)
	}
	session, err := db.TicketSession(ctx, version.ID, firstClaim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	remote := exec.Command("git", "remote", "set-url", "origin", "https://user:token@github.com/owner/repo.git")
	remote.Dir = session.WorkspacePath
	if output, err := remote.CombinedOutput(); err != nil {
		t.Fatalf("set credential-bearing remote: %v (%s)", err, output)
	}
	_, err = db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: firstClaim.TicketID, Owner: "agent-owner", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second),
	})
	if !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("claim while accepted candidate is waiting = %v", err)
	}
}

func TestControllerRejectsCredentialBearingWorkspacePushURL(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, firstClaim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	firstRuntime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "first"), ContainerID: "container-1"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: firstRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
	if _, err := controller.Run(ctx, candidateRequest(firstClaim, source, "ticket-1", "implement")); err != nil {
		t.Fatal(err)
	}
	session, err := db.TicketSession(ctx, version.ID, firstClaim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	pushURL := exec.Command("git", "remote", "set-url", "--push", "origin", "https://user:token@github.com/owner/repo.git")
	pushURL.Dir = session.WorkspacePath
	if output, err := pushURL.CombinedOutput(); err != nil {
		t.Fatalf("set credential-bearing push URL: %v (%s)", err, output)
	}
	_, err = db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: firstClaim.TicketID, Owner: "agent-owner", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second)})
	if !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("claim while accepted candidate is waiting = %v", err)
	}
}

func TestControllerRestoresAcceptedCommitWhenCandidateAcceptanceFails(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, firstClaim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	firstRuntime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "first"), ContainerID: "container-1"}}}
	firstController := agent.Controller{Store: db, Workspace: manager, Runtime: firstRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}}
	accepted, err := firstController.Run(ctx, candidateRequest(firstClaim, source, "ticket-1", "implement"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: firstClaim.TicketID, Owner: "agent-owner", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second),
	})
	if !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("claim while accepted candidate is waiting = %v, want fencing conflict", err)
	}
	session, err := db.TicketSession(ctx, version.ID, firstClaim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir = session.WorkspacePath
	output, err := head.Output()
	if err != nil || strings.TrimSpace(string(output)) != accepted.Commit {
		t.Fatalf("workspace head = %q, err = %v; want accepted %q", output, err, accepted.Commit)
	}
}

func codexOutput(sessionID, summary string) []byte {
	message, _ := json.Marshal(map[string]string{"summary": summary})
	item, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(message)}})
	return []byte(`{"type":"thread.started","thread_id":"` + sessionID + `"}` + "\n" + string(item))
}

func candidateRequest(claim store.TicketClaim, source, branch, prompt string) agent.RunRequest {
	return agent.RunRequest{Claim: claim, SourceRepository: source, Branch: branch, Prompt: prompt, Publication: store.CandidatePublication{Repository: "owner/repo", Branch: branch, ExpectRemoteAbsent: true, Title: claim.TicketTitle}}
}

func initRepository(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md", ".gitignore")
	run("commit", "-m", "base")
	return dir
}
