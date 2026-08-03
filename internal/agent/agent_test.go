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
	results         []worker.Result
	specs           []worker.Spec
	err             error
	dirty           bool
	failAfterCommit bool
	switchBranch    bool
	ignoredFile     bool
}

func (r *fakeRuntime) Run(_ context.Context, spec worker.Spec) (worker.Result, error) {
	r.specs = append(r.specs, spec)
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
	if _, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: source, Branch: "ticket-1", Prompt: "implement the ticket"}); err == nil {
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

func TestControllerPersistsCodexSessionAcrossReplacementRuns(t *testing.T) {
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
	candidate, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: source, Branch: "ticket-1", Prompt: "implement the ticket"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.CodexSessionID != "codex-session-1" || candidate.Commit == "" {
		t.Fatalf("candidate = %#v", candidate)
	}
	if len(first.specs) != 1 || strings.Join(first.specs[0].Command, " ") != "codex exec --json --output-schema "+filepath.Join(root, "codex", claim.SessionID, "output-schema.json")+" implement the ticket" {
		t.Fatalf("first worker spec = %#v", first.specs)
	}
	if first.specs[0].AgentIdentity == "" || len(first.specs[0].Mounts) != 2 || first.specs[0].Environment["GITHUB_TOKEN"] != "" {
		t.Fatalf("first worker isolation = %#v", first.specs[0])
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, filepath.Join(root, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	nextClaim, err := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: 1, Owner: "replacement-owner", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if nextClaim.SessionID != claim.SessionID {
		t.Fatalf("replacement session = %q, want %q", nextClaim.SessionID, claim.SessionID)
	}

	second := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session-1", "revised"), ContainerID: "container-2"}}}
	controller = agent.Controller{Store: db, Workspace: manager, Runtime: second, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0", "git": "2.0.0"}}
	if _, err := controller.Run(ctx, agent.RunRequest{Claim: nextClaim, SourceRepository: source, Branch: "ticket-1", Prompt: "review the implementation"}); err != nil {
		t.Fatal(err)
	}
	if len(second.specs) != 1 || strings.Join(second.specs[0].Command, " ") != "codex exec resume codex-session-1 --json --output-schema "+filepath.Join(root, "codex", claim.SessionID, "output-schema.json")+" review the implementation" {
		t.Fatalf("replacement worker spec = %#v", second.specs)
	}
	if second.specs[0].WorkspacePath != first.specs[0].WorkspacePath || second.specs[0].CodexStatePath != first.specs[0].CodexStatePath || second.specs[0].Branch != first.specs[0].Branch || second.specs[0].AgentIdentity != first.specs[0].AgentIdentity {
		t.Fatalf("replacement lost durable identity: first=%#v second=%#v", first.specs[0], second.specs[0])
	}

	session, err := db.TicketSession(ctx, version.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if session.CodexSessionID != "codex-session-1" || session.WorkspacePath != first.specs[0].WorkspacePath || session.Branch != "ticket-1" {
		t.Fatalf("persisted session = %#v", session)
	}
	if !json.Valid(candidate.StructuredOutput) {
		t.Fatalf("structured output is not JSON: %s", candidate.StructuredOutput)
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
			if _, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: source, Branch: "ticket-1", Prompt: "implement"}); err == nil {
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
	if _, err := controller.Run(ctx, agent.RunRequest{Claim: firstClaim, SourceRepository: source, Branch: "ticket-1", Prompt: "implement"}); err != nil {
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
	nextClaim, err := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: firstClaim.TicketID, Owner: "agent-owner", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondRuntime := &fakeRuntime{}
	controller.Runtime = secondRuntime
	_, err = controller.Run(ctx, agent.RunRequest{Claim: nextClaim, SourceRepository: source, Branch: "ticket-1", Prompt: "revise"})
	if err == nil || !strings.Contains(err.Error(), "absolute local path") {
		t.Fatalf("persisted external remote error = %v", err)
	}
	if len(secondRuntime.specs) != 0 {
		t.Fatal("worker started with a persisted external remote")
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
	if _, err := controller.Run(ctx, agent.RunRequest{Claim: firstClaim, SourceRepository: source, Branch: "ticket-1", Prompt: "implement"}); err != nil {
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
	nextClaim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: firstClaim.TicketID, Owner: "agent-owner", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	secondRuntime := &fakeRuntime{}
	controller.Runtime = secondRuntime
	if _, err := controller.Run(ctx, agent.RunRequest{Claim: nextClaim, SourceRepository: source, Branch: "ticket-1", Prompt: "revise"}); err == nil || !strings.Contains(err.Error(), "absolute local path") {
		t.Fatalf("persisted external push URL error = %v", err)
	}
	if len(secondRuntime.specs) != 0 {
		t.Fatal("worker started with a persisted external push URL")
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
	accepted, err := firstController.Run(ctx, agent.RunRequest{Claim: firstClaim, SourceRepository: source, Branch: "ticket-1", Prompt: "implement"})
	if err != nil {
		t.Fatal(err)
	}
	nextClaim, err := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: firstClaim.TicketID, Owner: "agent-owner", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondRuntime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "second"), ContainerID: "container-2"}}}
	secondController := agent.Controller{
		Store: db, Workspace: manager, Runtime: secondRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"},
		Now: func() time.Time { return nextClaim.LeaseExpiresAt.Add(time.Second) },
	}
	if _, err := secondController.Run(ctx, agent.RunRequest{Claim: nextClaim, SourceRepository: source, Branch: "ticket-1", Prompt: "revise"}); err == nil {
		t.Fatal("expired candidate acceptance succeeded")
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
	diagnostic, err := db.RunDiagnostic(ctx, nextClaim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(filepath.Dir(diagnostic), "revision.patch"))
	if err != nil || !strings.Contains(string(patch), "candidate resume") {
		t.Fatalf("unaccepted candidate evidence = %q, err = %v", patch, err)
	}
}

func codexOutput(sessionID, summary string) []byte {
	message, _ := json.Marshal(map[string]string{"summary": summary})
	item, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(message)}})
	return []byte(`{"type":"thread.started","thread_id":"` + sessionID + `"}` + "\n" + string(item))
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
