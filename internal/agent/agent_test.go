package agent_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/agent"
	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

type fakeRuntime struct {
	results                       []worker.Result
	specs                         []worker.Spec
	err                           error
	dirty                         bool
	failAfterCommit               bool
	switchBranch                  bool
	ignoredFile                   bool
	deliveryOutput                []byte
	deliveryDeadline              time.Time
	workspaceContent              string
	corruptCodexAuth              bool
	deleteCodexAuth               bool
	blockDiagnostics              bool
	refreshedAuth                 []byte
	deleteCodexAuthDuringDelivery bool
	beforeDeliveryReturn          func(time.Time) error
	deliveryErr                   error
	deliveryOrigin                string
}

type blockingFailureRuntime struct {
	started chan<- struct{}
	release <-chan struct{}
}

func testChatGPTAuth(accessToken string) []byte {
	return []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":"account","id_token":"id-token","refresh_token":"refresh-token"}}`, accessToken))
}

func (r blockingFailureRuntime) Run(_ context.Context, _ worker.Spec) (worker.Result, error) {
	r.started <- struct{}{}
	<-r.release
	return worker.Result{Output: []byte(`{"type":"thread.started","thread_id":"expired-agent"}`), ContainerID: "expired-container"}, errors.New("worker crashed")
}

func (r *fakeRuntime) Run(ctx context.Context, spec worker.Spec) (worker.Result, error) {
	r.specs = append(r.specs, spec)
	if spec.Command[0] == "no-mistakes" {
		origin := exec.Command("git", "-C", spec.WorkspacePath, "config", "--local", "--get-all", "remote.origin.url")
		originOutput, err := origin.Output()
		if err != nil {
			return worker.Result{}, err
		}
		r.deliveryOrigin = strings.TrimSpace(string(originOutput))
		if r.deliveryErr != nil {
			return worker.Result{}, r.deliveryErr
		}
		if r.deleteCodexAuthDuringDelivery {
			if err := os.Remove(filepath.Join(spec.CodexStatePath, "auth.json")); err != nil {
				return worker.Result{}, err
			}
		}
		output := []byte("run:\n  id: delivery-1\n  status: completed\noutcome: checks-passed\n")
		if len(r.deliveryOutput) > 0 {
			output = r.deliveryOutput
		}
		r.deliveryDeadline, _ = ctx.Deadline()
		result := worker.Result{Output: output, Stdout: output, ContainerID: "delivery-container"}
		if r.beforeDeliveryReturn != nil {
			if err := r.beforeDeliveryReturn(r.deliveryDeadline); err != nil {
				return result, err
			}
		}
		return result, nil
	}
	marker := "initial"
	if slices.Contains(spec.Command, "resume") {
		marker = "resume"
	}
	content := "candidate " + marker + "\n"
	if r.workspaceContent != "" {
		content = r.workspaceContent
	}
	if err := os.WriteFile(filepath.Join(spec.WorkspacePath, "agent.txt"), []byte(content), 0o644); err != nil {
		return worker.Result{}, err
	}
	if r.corruptCodexAuth {
		if err := os.WriteFile(filepath.Join(spec.CodexStatePath, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"truncated"}}`), 0o600); err != nil {
			return worker.Result{}, err
		}
	}
	if len(r.refreshedAuth) > 0 {
		if err := os.WriteFile(filepath.Join(spec.CodexStatePath, "auth.json"), r.refreshedAuth, 0o600); err != nil {
			return worker.Result{}, err
		}
	}
	if r.deleteCodexAuth {
		if err := os.Remove(filepath.Join(spec.CodexStatePath, "auth.json")); err != nil {
			return worker.Result{}, err
		}
	}
	if r.blockDiagnostics {
		if err := os.WriteFile(filepath.Join(filepath.Dir(spec.CodexStatePath), "diagnostics"), []byte("not a directory"), 0o600); err != nil {
			return worker.Result{}, err
		}
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
	for _, output := range []*[]byte{&result.Output, &result.Stdout} {
		if !bytes.Contains(*output, []byte(candidateCommitPlaceholder)) {
			continue
		}
		head := exec.Command("git", "rev-parse", "HEAD")
		head.Dir = spec.WorkspacePath
		commit, err := head.Output()
		if err != nil {
			return worker.Result{}, err
		}
		*output = bytes.ReplaceAll(*output, []byte(candidateCommitPlaceholder), []byte(strings.TrimSpace(string(commit))))
	}
	return result, nil
}

func TestControllerCreatesIndependentWorkspaceObjectCopies(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	runtime := &fakeRuntime{dirty: true, err: errors.New("stop after workspace creation"), results: []worker.Result{{ContainerID: "container-failed"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	_, runErr := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "create the workspace"))
	if runErr == nil {
		t.Fatal("failed worker run returned nil error")
	}
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if session.WorkspacePath == "" {
		t.Fatalf("Ticket Workspace was not bound: %v", runErr)
	}
	origin := exec.Command("git", "remote", "get-url", "origin")
	origin.Dir = session.WorkspacePath
	originOutput, err := origin.CombinedOutput()
	if err != nil {
		t.Fatalf("workspace origin: %v (%s)", err, originOutput)
	}
	originPath := strings.TrimSpace(string(originOutput))
	if !filepath.IsAbs(originPath) || !strings.EqualFold(filepath.Clean(originPath), filepath.Clean(source)) {
		t.Fatalf("workspace origin = %q, want absolute local source %q", originPath, source)
	}
	relativeObjects := looseObjects(t, source)
	for _, relativeObject := range relativeObjects {
		sourceInfo, err := os.Stat(filepath.Join(source, ".git", "objects", relativeObject))
		if err != nil {
			t.Fatal(err)
		}
		workspaceInfo, err := os.Stat(filepath.Join(session.WorkspacePath, ".git", "objects", relativeObject))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(sourceInfo, workspaceInfo) {
			t.Fatalf("Ticket Workspace shares Git object %q with its source repository", relativeObject)
		}
	}
	t.Logf("Controller.Run created Ticket Workspace %q from local origin %q with %d independently copied Git objects (os.SameFile=false for every object)", session.WorkspacePath, originPath, len(relativeObjects))
}

func TestControllerCreatesLFOnlyTicketWorkspaceDespiteHostAutoCRLF(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	globalConfig := filepath.Join(root, "global.gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[core]\n\tautocrlf = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	runtime := &fakeRuntime{dirty: true, err: errors.New("stop after workspace creation"), results: []worker.Result{{ContainerID: "container-failed"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	_, runErr := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "create the workspace"))
	if runErr == nil {
		t.Fatal("failed worker run returned nil error")
	}
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if session.WorkspacePath == "" {
		t.Fatalf("Ticket Workspace was not bound: %v", runErr)
	}
	readme, err := os.ReadFile(filepath.Join(session.WorkspacePath, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(readme), "\r\n") {
		t.Fatalf("Ticket Workspace README uses CRLF: %q", readme)
	}
	for key, want := range map[string]string{
		"core.autocrlf":  "false",
		"core.eol":       "lf",
		"core.longpaths": "true",
		"user.name":      "workflow-ticket-agent",
		"user.email":     "workflow-ticket-agent@users.noreply.github.com",
	} {
		command := exec.Command("git", "config", "--local", "--get", key)
		command.Dir = session.WorkspacePath
		output, err := command.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != want {
			t.Fatalf("Ticket Workspace %s = %q, err=%v; want %q", key, output, err, want)
		}
	}
}

func TestControllerNormalizesExistingCRLFTicketWorkspaceDuringRecovery(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	globalConfig := filepath.Join(root, "global.gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[core]\n\tautocrlf = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	workspacePath := filepath.Join(root, "workspaces", claim.SessionID)
	if err := os.MkdirAll(filepath.Dir(workspacePath), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"clone", "--local", "--no-hardlinks", source, workspacePath}, {"-C", workspacePath, "checkout", "-b", "ticket-1"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	for key, value := range map[string]string{"user.name": "repository-owner", "user.email": "owner@example.com"} {
		if output, err := exec.Command("git", "-C", workspacePath, "config", "--local", key, value).CombinedOutput(); err != nil {
			t.Fatalf("configure existing workspace %s: %v\n%s", key, err, output)
		}
	}
	readmePath := filepath.Join(workspacePath, "README.md")
	before, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "\r\n") {
		t.Fatalf("test setup did not create CRLF: %q", before)
	}

	runtime := &fakeRuntime{}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "recover the workspace")); err == nil || !strings.Contains(err.Error(), "workspace was not clean") {
		t.Fatalf("CRLF recovery error = %v", err)
	}
	if len(runtime.specs) != 0 {
		t.Fatal("Worker launched before CRLF workspace recovery")
	}
	after, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "\r\n") {
		t.Fatalf("recovered Ticket Workspace still uses CRLF: %q", after)
	}
	for key, want := range map[string]string{"user.name": "workflow-ticket-agent", "user.email": "workflow-ticket-agent@users.noreply.github.com"} {
		command := exec.Command("git", "config", "--local", "--get", key)
		command.Dir = workspacePath
		output, err := command.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != want {
			t.Fatalf("recovered Ticket Workspace %s = %q, err=%v; want %q", key, output, err, want)
		}
	}
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = workspacePath
	output, err := status.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "" {
		t.Fatalf("recovered Ticket Workspace status = %q, err=%v", output, err)
	}
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil || session.WorkspacePath != workspacePath {
		t.Fatalf("Ticket Session = %#v, err=%v", session, err)
	}
}

func TestControllerSeedsCodexAuthenticationBeforeFirstWorkerRun(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	authSource := filepath.Join(root, "host-auth.json")
	want := testChatGPTAuth("test-only")
	if err := os.WriteFile(authSource, want, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{
		RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"),
		CodexAuthFile: authSource,
	}
	db, version, claim := createClaimWithProvisioner(t, ctx, root, manager.ProvisionCodexSession)
	defer db.Close()
	runtime := &fakeRuntime{dirty: true, err: errors.New("stop after auth observation"), results: []worker.Result{{ContainerID: "container-failed"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "observe auth")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(session.CodexStatePath, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("seeded auth = %q, want exact source bytes", got)
	}
	t.Logf("claim attempt=%d returned only after the host ChatGPT cache was seeded into Ticket Session %s", claim.Attempt, claim.SessionID)
}

func TestControllerPreservesExistingTicketSessionAuthentication(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "codex", claim.SessionID)
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
	want := testChatGPTAuth("session-refreshed")
	if err := os.WriteFile(filepath.Join(statePath, "auth.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{
		RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"),
		CodexAuthFile: authSource,
	}
	runtime := &fakeRuntime{dirty: true, err: errors.New("stop after auth observation"), results: []worker.Result{{ContainerID: "container-failed"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "observe auth")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	got, err := os.ReadFile(filepath.Join(statePath, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("existing Ticket Session auth was overwritten: got %q", got)
	}
	t.Logf("Ticket Session %s retained its refreshed auth cache instead of being overwritten from the host source", claim.SessionID)
}

func TestControllerRejectsNonChatGPTAuthenticationBeforeWorkerLaunch(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, []byte(`{"auth_mode":"api","OPENAI_API_KEY":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{dirty: true, err: errors.New("worker should not start"), results: []worker.Result{{ContainerID: "unexpected"}}}
	controller := agent.Controller{
		Store: db,
		Workspace: agent.WorkspaceManager{
			RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource,
		},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if err := controller.Workspace.ProvisionCodexAuthentication(ctx, claim.SessionID, false); err == nil || !strings.Contains(err.Error(), "valid ChatGPT login cache") {
		t.Fatalf("invalid authentication provisioning error = %v", err)
	}
	_, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "must not launch"))
	if err == nil || !strings.Contains(err.Error(), "authentication cache is unavailable") {
		t.Fatalf("invalid authentication error = %v", err)
	}
	if len(runtime.specs) != 0 {
		t.Fatalf("worker launched with non-ChatGPT authentication: %#v", runtime.specs)
	}
}

func TestControllerRejectsIncompleteChatGPTAuthenticationBeforeWorkerLaunch(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, []byte(`{"auth_mode":"chatgpt","tokens":{"junk":null}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{dirty: true, err: errors.New("worker should not start"), results: []worker.Result{{ContainerID: "unexpected"}}}
	controller := agent.Controller{
		Store: db,
		Workspace: agent.WorkspaceManager{
			RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource,
		},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if err := controller.Workspace.ProvisionCodexAuthentication(ctx, claim.SessionID, false); err == nil || !strings.Contains(err.Error(), "valid ChatGPT login cache") {
		t.Fatalf("incomplete authentication provisioning error = %v", err)
	}
	_, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "must not launch"))
	if err == nil || !strings.Contains(err.Error(), "authentication cache is unavailable") {
		t.Fatalf("incomplete authentication error = %v", err)
	}
	if len(runtime.specs) != 0 {
		t.Fatalf("worker launched with incomplete ChatGPT authentication: %#v", runtime.specs)
	}
}

func TestControllerRedactsCodexCredentialsFromAllFailureDiagnostics(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	secret := "diagnostic-secret-token"
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{
		dirty: true, err: errors.New("worker failed"), workspaceContent: "copied credential: " + secret,
		results: []worker.Result{{Output: []byte("worker output credential: " + secret), ContainerID: "container-failed"}},
	}
	controller := agent.Controller{
		Store: db,
		Workspace: agent.WorkspaceManager{
			RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource,
		},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if err := controller.Workspace.ProvisionCodexAuthentication(ctx, claim.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "fail safely")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(filepath.Dir(diagnostic), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), secret) {
			t.Errorf("diagnostic artifact %s leaked a Codex credential", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceManagerAdmitsExistingSessionAuthenticationWithoutHostSource(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	stateRoot := filepath.Join(root, "codex")
	statePath := filepath.Join(stateRoot, claim.SessionID)
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "auth.json"), testChatGPTAuth("session-current"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{
		RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: stateRoot,
		CodexAuthFile: filepath.Join(root, "missing-host-auth.json"),
	}
	if err := manager.AdmitCodexAuthentication(ctx, db, claim.VersionID, claim.TicketID); err != nil {
		t.Fatalf("existing Ticket Session authentication was coupled to host source: %v", err)
	}
}

func TestWorkspaceManagerDoesNotReseedMissingEstablishedSessionAuthentication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth("host-current"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource}
	if err := manager.ProvisionCodexAuthentication(ctx, "ts-established", true); err == nil {
		t.Fatal("missing established Session authentication was reseeded")
	}
	if _, err := os.Stat(filepath.Join(root, "codex", "ts-established", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("established Session authentication was recreated: %v", err)
	}
}

func TestWorkspaceManagerRejectsOverlappingWorkspaceAndCodexState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth("host-current"), 0o600); err != nil {
		t.Fatal(err)
	}
	sharedRoot := filepath.Join(root, "shared")
	manager := agent.WorkspaceManager{RootDir: sharedRoot, CodexStateRoot: sharedRoot, CodexAuthFile: authSource}
	if err := manager.ProvisionCodexAuthentication(ctx, "ts-new", false); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlapping workspace and state error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(sharedRoot, "ts-new", "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials were written into overlapping workspace: %v", err)
	}
}

func TestWorkspaceManagerRollbackRemovesUncommittedSessionState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth("host-current"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource}
	result, err := manager.ProvisionCodexSession(ctx, store.SessionProvisioning{SessionID: "ts-uncommitted"})
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "codex", "ts-uncommitted")
	if err := os.WriteFile(filepath.Join(state, "uncommitted-state"), []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result.Rollback == nil {
		t.Fatal("new Session provisioning did not return rollback")
	}
	if err := result.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted Session state survived rollback: %v", err)
	}
}

func TestControllerRedactsPreAndPostRefreshCredentials(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	oldSecret := "credential-before-refresh"
	newSecret := "credential-after-refresh"
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth(oldSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource}
	if err := manager.ProvisionCodexAuthentication(ctx, claim.SessionID, false); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{
		dirty:         true,
		err:           errors.New("worker failed after refresh"),
		refreshedAuth: testChatGPTAuth(newSecret),
		results:       []worker.Result{{Output: []byte(oldSecret + " " + newSecret), ContainerID: "container-refreshed"}},
	}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "refresh then fail")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(report), oldSecret) || strings.Contains(string(report), newSecret) {
		t.Fatalf("refresh credentials leaked through diagnostic: %s", report)
	}
}

func TestControllerBacksOffInitialDeliverySourceRefreshFailure(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	failureNow := claim.LeaseExpiresAt.Add(-30 * time.Second)
	manager := agent.WorkspaceManager{
		RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"),
		RefreshDeliverySource: func(context.Context, string) (string, error) {
			return "", errors.New("GitHub source temporarily unavailable")
		},
	}
	runtime := &fakeRuntime{}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, Now: func() time.Time { return failureNow }}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "GitHub source temporarily unavailable") {
		t.Fatalf("initial Delivery Source refresh error = %v", err)
	}
	if len(runtime.specs) != 0 {
		t.Fatal("worker started after initial Delivery Source refresh failed")
	}
	if _, err := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: claim.TicketID, Owner: "replacement", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: failureNow.Add(30 * time.Second),
	}); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("initial Delivery Source infrastructure retry = %v, want ErrNotReady", err)
	}
}

func TestControllerAcceptsCredentialBearingCandidateInTrustedWorkflow(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	secret := "candidate-auth-secret"
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource}
	db, _, claim := createClaimWithProvisioner(t, ctx, root, manager.ProvisionCodexSession)
	defer db.Close()
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-secret", "implemented with "+secret), ContainerID: "container-secret"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, GatewayURL: "http://gateway.test"}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement in trusted workflow")); err != nil {
		t.Fatalf("credential-bearing Candidate was rejected: %v", err)
	}
	candidate, err := db.CandidateRevision(ctx, claim.RunID)
	if err != nil {
		t.Fatalf("credential-bearing Candidate was not persisted: %v", err)
	}
	if !strings.Contains(string(candidate.StructuredOutput), secret) {
		t.Fatalf("trusted Candidate output = %q, want preserved credential-bearing summary", candidate.StructuredOutput)
	}
	if len(runtime.specs) != 2 {
		t.Fatalf("credential-bearing Candidate launched %d containers, want Worker and Delivery Controller", len(runtime.specs))
	}
}

func TestExpiredRunWithMissingAuthenticationFailsDurably(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth("expired-auth-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource}
	db, version, claim := createClaimWithProvisioner(t, ctx, root, manager.ProvisionCodexSession)
	defer db.Close()
	if err := os.Remove(filepath.Join(root, "codex", claim.SessionID, "auth.json")); err != nil {
		t.Fatal(err)
	}
	_, err := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: claim.TicketID, Owner: "replacement", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: claim.LeaseExpiresAt.Add(time.Second), ProvisionSession: manager.ProvisionCodexSession,
	})
	if !errors.Is(err, store.ErrSessionAuthenticationUnavailable) {
		t.Fatalf("missing expired authentication error = %v", err)
	}
	diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
	if err != nil {
		t.Fatalf("expired Run diagnostic = %v", err)
	}
	report, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "detailed evidence omitted") {
		t.Fatalf("expired Run diagnostic = %q", report)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, claim.TicketID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired Run remained current: %v", err)
	}
	if _, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: claim.TicketID, Owner: "replacement", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: claim.LeaseExpiresAt.Add(2 * time.Second), ProvisionSession: manager.ProvisionCodexSession}); !errors.Is(err, store.ErrFencingConflict) {
		t.Fatalf("authentication-corrupt Session was reclaimed: %v", err)
	}
	t.Logf("corrupt authentication terminalized Run %s, removed its current claim, and blocked a replacement claim", claim.RunID)
}

func TestControllerRecordsMinimalDiagnosticWhenCodexAuthenticationCannotBeRedacted(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{
		dirty: true, corruptCodexAuth: true, err: errors.New("worker failed"),
		results: []worker.Result{{Output: []byte("sensitive worker output must be omitted"), ContainerID: "container-failed"}},
	}
	controller := agent.Controller{
		Store: db,
		Workspace: agent.WorkspaceManager{
			RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource,
		},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if err := controller.Workspace.ProvisionCodexAuthentication(ctx, claim.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "fail after auth corruption")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
	if err != nil {
		t.Fatalf("failed Run did not record a minimal diagnostic: %v", err)
	}
	report, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "detailed evidence omitted") || strings.Contains(string(report), "sensitive worker output") {
		t.Fatalf("unsafe or incomplete minimal diagnostic:\n%s", report)
	}
	entries, err := os.ReadDir(filepath.Dir(diagnostic))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "report.txt" {
		t.Fatalf("minimal diagnostic persisted unsafe evidence: %#v", entries)
	}
	projection, err := db.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tickets) == 0 || projection.Tickets[0].State != "Needs Attention" {
		t.Fatalf("authentication-corrupt Run projection = %#v", projection.Tickets)
	}
	audit, err := db.WorkerAudit(ctx, claim.RunID)
	if err != nil || audit.ContainerID != "container-failed" {
		t.Fatalf("authentication-corrupt Run audit = %#v, %v", audit, err)
	}
}

func TestControllerAuditsDeliveryBeforePostRunAuthenticationInspection(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth("delivery-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{
		results:                       []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}},
		deleteCodexAuthDuringDelivery: true,
	}
	controller := agent.Controller{
		Store: db,
		Workspace: agent.WorkspaceManager{
			RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource,
		},
		Runtime: runtime, GatewayURL: "http://gateway.test",
	}
	if err := controller.Workspace.ProvisionCodexAuthentication(ctx, claim.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "authentication cache is unavailable") {
		t.Fatalf("post-delivery authentication error = %v", err)
	}
	if len(runtime.specs) != 2 {
		t.Fatalf("runtime specs = %#v", runtime.specs)
	}
	audit, err := db.WorkerAudit(ctx, runtime.specs[1].RunID)
	if err != nil {
		t.Fatal(err)
	}
	if audit.ContainerID != "delivery-container" || audit.ImageDigest != runtime.specs[1].ImageDigest || audit.GitHubWriteCredentials || !strings.Contains(audit.ExtraHostsJSON, worker.GatewayHostMapping) || !strings.Contains(audit.ToolVersionsJSON, `"github-cli":"2.97.0"`) {
		t.Fatalf("post-delivery authentication audit = %#v", audit)
	}
}

func TestControllerAuditsDeliveryAfterRecoveryExpiresLease(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}}}
	runtime.beforeDeliveryReturn = func(deadline time.Time) error {
		if deadline.IsZero() {
			return errors.New("Delivery Controller deadline is missing")
		}
		runs, err := db.ActiveRecoveryRuns(context.Background(), version.ID, deadline.Add(time.Second))
		if err != nil {
			return err
		}
		if len(runs) != 0 {
			return fmt.Errorf("active recovery runs after expiry = %#v", runs)
		}
		return nil
	}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, GatewayURL: "http://gateway.test",
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); !errors.Is(err, store.ErrInvalidClaim) {
		t.Fatalf("expired Delivery Controller completion = %v, want active-lease rejection", err)
	}
	if len(runtime.specs) != 2 {
		t.Fatalf("runtime specs = %#v", runtime.specs)
	}
	deliverySpec := runtime.specs[1]
	deliveryGeneration := claim.LeaseGeneration + 1
	audit, err := db.WorkerAudit(ctx, deliverySpec.RunID)
	if err != nil {
		t.Fatalf("expired Delivery Controller audit: %v", err)
	}
	if audit.ContainerID != "delivery-container" || audit.ImageDigest != deliverySpec.ImageDigest || audit.GitHubWriteCredentials || !strings.Contains(audit.ExtraHostsJSON, worker.GatewayHostMapping) {
		t.Fatalf("expired Delivery Controller audit = %#v", audit)
	}
	if err := db.RecordWorkerContainer(ctx, deliverySpec.RunID, deliveryGeneration, "replacement-container"); err == nil {
		t.Fatal("expired Delivery Controller audit was mutable")
	}
	raw, err := sql.Open("sqlite", filepath.Join(root, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var auditCount int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_audits WHERE run_id = ?`, deliverySpec.RunID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("expired Delivery Controller audits = %d, want 1", auditCount)
	}
	var runState, leaseState string
	if err := raw.QueryRowContext(ctx, `SELECT r.state, l.state FROM worker_runs r JOIN run_leases l ON l.run_id = r.run_id AND l.generation = r.lease_generation WHERE r.run_id = ?`, deliverySpec.RunID).Scan(&runState, &leaseState); err != nil {
		t.Fatal(err)
	}
	if runState != "failed" || leaseState != "expired" {
		t.Fatalf("expired Delivery Controller state = %q/%q", runState, leaseState)
	}
}

func TestControllerPersistsDeliveryAuditBeforePostDockerProcessLoss(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}}}
	runtime.beforeDeliveryReturn = func(time.Time) error {
		deliverySpec := runtime.specs[1]
		audit, err := db.WorkerAudit(context.Background(), deliverySpec.RunID)
		if err != nil {
			t.Fatalf("audit was absent after Docker start: %v", err)
		}
		if audit.ContainerID != "" || audit.LeaseGeneration != claim.LeaseGeneration+1 || audit.ImageDigest != deliverySpec.ImageDigest || audit.GitHubWriteCredentials || !strings.Contains(audit.MountsJSON, `"/workspace"`) || !strings.Contains(audit.ExtraHostsJSON, worker.GatewayHostMapping) || !strings.Contains(audit.ToolVersionsJSON, `"github-cli":"2.97.0"`) {
			t.Fatalf("pre-return Delivery Controller audit = %#v", audit)
		}
		panic("control plane process lost")
	}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, GatewayURL: "http://gateway.test",
	}
	var processLost bool
	func() {
		defer func() {
			processLost = recover() == "control plane process lost"
		}()
		_, _ = controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement"))
	}()
	if !processLost || len(runtime.specs) != 2 {
		t.Fatalf("post-Docker process loss = %v, specs = %#v", processLost, runtime.specs)
	}
	deliverySpec := runtime.specs[1]
	deliveryGeneration := claim.LeaseGeneration + 1
	if err := db.RecordWorkerContainer(ctx, deliverySpec.RunID, deliveryGeneration, "reconciled-container"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordWorkerContainer(ctx, deliverySpec.RunID, deliveryGeneration, "reconciled-container"); err != nil {
		t.Fatalf("idempotent container result: %v", err)
	}
	if err := db.RecordWorkerContainer(ctx, deliverySpec.RunID, deliveryGeneration, "different-container"); err == nil {
		t.Fatal("container result was mutable")
	}
	raw, err := sql.Open("sqlite", filepath.Join(root, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var auditCount, resultCount int
	if err := raw.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM worker_audits WHERE run_id = ?), (SELECT COUNT(*) FROM worker_container_results WHERE run_id = ?)`, deliverySpec.RunID, deliverySpec.RunID).Scan(&auditCount, &resultCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || resultCount != 1 {
		t.Fatalf("post-loss audit/result counts = %d/%d, want 1/1", auditCount, resultCount)
	}
}

func TestControllerRecordsRunFailureWhenDiagnosticFilesystemIsUnavailable(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{
		dirty: true, blockDiagnostics: true, err: errors.New("worker failed"),
		results: []worker.Result{{Output: []byte("diagnostic storage unavailable"), ContainerID: "container-failed"}},
	}
	controller := agent.Controller{
		Store: db,
		Workspace: agent.WorkspaceManager{
			RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"),
		},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "fail without diagnostics")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	if _, err := db.CurrentClaim(ctx, version.ID, claim.TicketID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Run remained active after diagnostic failure: %v", err)
	}
	if _, err := db.RunDiagnostic(ctx, claim.RunID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed diagnostic path was persisted: %v", err)
	}
	t.Logf("Run %s left active state even though diagnostic storage was unavailable; no diagnostic record was required", claim.RunID)
}

func TestControllerOmitsDetailedDiagnosticsWhenWorkerDeletesCodexAuthentication(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	secret := "deleted-auth-secret"
	authSource := filepath.Join(root, "host-auth.json")
	if err := os.WriteFile(authSource, testChatGPTAuth(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{
		deleteCodexAuth: true,
		results:         []worker.Result{{Output: append(codexOutput("codex-clean", "implemented"), []byte("\ncredential read before deletion: "+secret)...), ContainerID: "container-clean"}},
	}
	controller := agent.Controller{
		Store: db,
		Workspace: agent.WorkspaceManager{
			RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex"), CodexAuthFile: authSource,
		},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if err := controller.Workspace.ProvisionCodexAuthentication(ctx, claim.SessionID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "fail after auth deletion")); err == nil {
		t.Fatal("failed worker run returned nil error")
	}
	diagnostic, err := db.RunDiagnostic(ctx, claim.RunID)
	if err != nil {
		t.Fatalf("failed Run did not record a minimal diagnostic: %v", err)
	}
	report, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "detailed evidence omitted") || strings.Contains(string(report), secret) {
		t.Fatalf("deleted credential leaked through diagnostic:\n%s", report)
	}
	entries, err := os.ReadDir(filepath.Dir(diagnostic))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "report.txt" {
		t.Fatalf("minimal diagnostic persisted unsafe evidence: %#v", entries)
	}
}

func TestControllerSnapshotsAndRestoresAnAbnormalWorkerRun(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	runtime := &fakeRuntime{dirty: true, ignoredFile: true, err: errors.New("worker crashed"), results: []worker.Result{{Output: []byte(`{"type":"thread.started","thread_id":"codex-failed"}` + "\n" + `{"type":"result","summary":"partial"}`), ContainerID: "container-failed"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
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
	expired := agent.Controller{Store: db, Workspace: manager, Runtime: blockingFailureRuntime{started: started, release: release}, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
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
	replacementController := agent.Controller{Store: db, Workspace: manager, Runtime: replacementRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
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
	return createClaimWithProvisioner(t, ctx, root, nil)
}

func createClaimWithProvisioner(t *testing.T, ctx context.Context, root string, provisioner store.SessionProvisioner) (*store.Store, store.PlanVersion, store.TicketClaim) {
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
	activateTestWorker(t, ctx, db)
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{VersionID: version.ID, TicketID: 1, Owner: "agent-owner", MaxParallelRuns: 1, LeaseTTL: time.Minute, Now: time.Now().UTC(), ProvisionSession: provisioner})
	if err != nil {
		t.Fatal(err)
	}
	return db, version, claim
}

func TestControllerRejectsImplementationCandidateWithNullCommit(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutputWithCommit("codex-session", "implemented", nil), ContainerID: "container-1"}}}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "structured result must name the workspace HEAD commit") {
		t.Fatalf("null implementation commit error = %v", err)
	}
}

func TestControllerDelegatesDeliveryCycleToNoMistakes(t *testing.T) {
	ctx := context.Background()
	repository := initRepository(t)
	if output, err := exec.Command("git", "-C", repository, "checkout", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("detach source repository: %v\n%s", err, output)
	}
	source := filepath.Join(t.TempDir(), "linked-worktree")
	if output, err := exec.Command("git", "-C", repository, "worktree", "add", source, "main").CombinedOutput(); err != nil {
		t.Fatalf("create linked source worktree: %v\n%s", err, output)
	}
	if output, err := exec.Command("git", "-C", source, "config", "--local", "credential.helper", "source-secret-helper").CombinedOutput(); err != nil {
		t.Fatalf("configure source credential helper: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(source, "untracked-credential.txt"), []byte("source-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	activateTestWorker(t, ctx, db)
	claim, err := db.ClaimReady(ctx, store.ClaimRequest{
		VersionID: version.ID, TicketID: 1, Owner: "agent-owner", MaxParallelRuns: 1,
		LeaseTTL: time.Minute, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	first := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session-1", "implemented"), ContainerID: "container-1"}}}
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: first, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0", "git": "2.0.0"}, GatewayURL: "http://gateway.test"}
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
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil || session.AcceptedCandidateRunID != claim.RunID {
		t.Fatalf("persisted Revision Round = %#v, err=%v", session, err)
	}
	if keys, err := db.DueDeliveryOutboxKeys(ctx, time.Now().UTC(), 8); err != nil || len(keys) != 0 {
		t.Fatalf("candidate acceptance queued delivery commands: keys=%#v, err=%v", keys, err)
	}
	if len(first.specs) != 2 || strings.Join(first.specs[1].Command, " ") != "no-mistakes axi run --intent implement the ticket" {
		t.Fatalf("first worker spec = %#v", first.specs)
	}
	if command := first.specs[0].Command; len(command) != 9 || command[0] != "codex" || command[1] != "exec" || command[2] != "--sandbox" || command[3] != "danger-full-access" || command[4] != "--json" || command[5] != "--output-schema" || command[7] != "--skip-git-repo-check" || command[8] != "implement the ticket" {
		t.Fatalf("Codex command = %#v", command)
	} else if schema, err := os.ReadFile(command[6]); err != nil || !strings.Contains(string(schema), `"summary"`) {
		t.Fatalf("Candidate output schema = %q, err = %v", schema, err)
	}
	if first.specs[0].AgentIdentity == "" || len(first.specs[0].Mounts) != 2 || first.specs[0].Environment["GITHUB_TOKEN"] != "" {
		t.Fatalf("first worker isolation = %#v", first.specs[0])
	}
	if first.specs[0].Environment["NO_MISTAKES_RUN_ID"] != "" {
		t.Fatalf("Codex worker received Delivery Controller environment = %#v", first.specs[0].Environment)
	}
	t.Log("Ticket Agent Worker launched with no GitHub token and can reach GitHub writes only through the credential-isolated Gateway")
	if first.specs[0].ImageDigest != "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("worker did not use Active Worker Image: %#v", first.specs[0])
	}
	if first.specs[1].Environment["NO_MISTAKES_RUN_ID"] == claim.RunID || first.specs[1].Environment["NO_MISTAKES_LEASE_TOKEN"] == claim.LeaseToken || first.specs[1].Environment["NO_MISTAKES_LEASE_GENERATION"] != fmt.Sprint(claim.LeaseGeneration+1) || first.specs[1].Environment["NO_MISTAKES_REPOSITORY"] != "owner/repo" || first.specs[1].Environment["NO_MISTAKES_BRANCH"] != "ticket-1" || first.specs[1].Environment["NO_MISTAKES_COMMIT_SHA"] != candidate.Commit {
		t.Fatalf("Delivery Controller Gateway fence environment = %#v", first.specs[1].Environment)
	}
	assertWorkflowDeliveryEnvironment(t, first.specs[1], claim.SessionID, claim.RunID, first.specs[1].RunID)
	if first.specs[1].Environment["NO_MISTAKES_GATEWAY_URL"] != "http://gateway.test" {
		t.Fatalf("Delivery Controller Gateway URL = %#v", first.specs[1].Environment)
	}
	assertDeliveryOriginMount(t, first, first.specs[1], source)
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
	assertWorkflowDeliveryEnvironment(t, second.specs[1], claim.SessionID, revision.RunID, second.specs[1].RunID)
	if command := second.specs[0].Command; len(command) != 11 || command[0] != "codex" || command[1] != "exec" || command[2] != "--sandbox" || command[3] != "danger-full-access" || command[4] != "resume" || command[5] != "--json" || command[6] != "--output-schema" || command[8] != "--skip-git-repo-check" || command[9] != "codex-session-1" || command[10] != "address the review feedback" {
		t.Fatalf("resumed Codex command = %#v", command)
	}
	if !json.Valid(candidate.StructuredOutput) {
		t.Fatalf("structured output is not JSON: %s", candidate.StructuredOutput)
	}
}

func TestControllerRequiresGatewayBeforeCandidateAcceptance(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}}}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"},
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "Gateway URL is required") {
		t.Fatalf("missing Gateway URL error = %v", err)
	}
	if len(runtime.specs) != 1 {
		t.Fatalf("worker specs = %#v, want only the Agent runtime", runtime.specs)
	}
	if _, err := db.CandidateRevision(ctx, claim.RunID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Candidate accepted without a Gateway URL: %v", err)
	}
}

func TestControllerRetryDeliveryRejectsAgentLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: &fakeRuntime{}, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if err := controller.RetryDelivery(ctx, claim); !errors.Is(err, store.ErrInvalidClaim) {
		t.Fatalf("retry with Agent lease = %v, want ErrInvalidClaim", err)
	}
	if len(controller.Runtime.(*fakeRuntime).specs) != 0 {
		t.Fatal("Delivery Controller launched for an Agent lease")
	}
}

func TestControllerRetryDeliveryPreservesCandidateRuntimeForOriginalReadyRunAfterRestart(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	t.Cleanup(func() { _ = db.Close() })
	workspacePath := filepath.Join(root, "workspace")
	for _, command := range [][]string{
		{"clone", "--config", "core.autocrlf=false", "--no-hardlinks", source, workspacePath},
		{"-C", workspacePath, "checkout", "-b", "ticket-1"},
		{"-C", workspacePath, "config", "user.name", "Test"},
		{"-C", workspacePath, "config", "user.email", "test@example.com"},
	} {
		if output, err := exec.Command("git", command...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", command, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "candidate.txt"), []byte("accepted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"-C", workspacePath, "add", "candidate.txt"}, {"-C", workspacePath, "commit", "-m", "candidate"}} {
		if output, err := exec.Command("git", command...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", command, err, output)
		}
	}
	head, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	codexStatePath := filepath.Join(root, "codex", claim.SessionID)
	if err := os.MkdirAll(codexStatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent-" + claim.SessionID, WorkspacePath: workspacePath, CodexStatePath: codexStatePath, Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	oldImage := "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oldTools := map[string]string{"codex": "1.0.0", "go": "1.25.12", "no-mistakes": "v1.0.0"}
	deliveryClaim, err := db.AcceptCandidateForDelivery(ctx, store.CandidateRevision{
		RunID: claim.RunID, LeaseToken: claim.LeaseToken, CodexSessionID: "codex-session", CommitSHA: strings.TrimSpace(string(head)),
		StructuredOutput: []byte(`{"summary":"accepted","checks":[{"command":"go test","outcome":"passed"}]}`), ImageDigest: oldImage, ToolVersions: oldTools, Now: time.Now().UTC(),
		Publication: store.CandidatePublication{Repository: "owner/repo", Branch: "ticket-1", ExpectRemoteAbsent: true, Title: "ticket"},
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(root, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"DELETE FROM schema_migrations WHERE version >= 48",
		"ALTER TABLE worker_runs DROP COLUMN delivery_runtime_candidate_run_id",
		"ALTER TABLE worker_audits DROP COLUMN lease_generation",
		"DROP TABLE worker_container_results",
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			raw.Close()
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, filepath.Join(root, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateWorkerRelease(ctx, store.WorkerRelease{
		Version: "0.2.0", SourceCommit: "cccccccccccccccccccccccccccccccccccccccc",
		ImageReference: "ghcr.io/owner/worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ManifestJSON:   `{"schema_version":1,"codex_version":"2.0.0","github_cli_version":"2.98.0","go_version":"1.25.12","no_mistakes_version":"v2.0.0"}`, VerifiedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: root, CodexStateRoot: filepath.Join(root, "codex")}, Runtime: runtime, GatewayURL: "http://gateway.test", SourceRepository: source}
	if err := controller.RetryDelivery(ctx, deliveryClaim); err != nil {
		t.Fatalf("resume original ready Delivery Worker Run: %v", err)
	}
	if len(runtime.specs) != 1 || runtime.specs[0].ImageDigest != oldImage || runtime.specs[0].ToolVersions["github-cli"] != "2.97.0" {
		t.Fatalf("original Delivery Worker runtime = %#v, want Candidate runtime", runtime.specs)
	}
	assertDeliveryOriginMount(t, runtime, runtime.specs[0], source)
	_, candidateTools, err := db.CandidateWorkerRuntime(ctx, deliveryClaim.VersionID, deliveryClaim.TicketID)
	if err != nil || candidateTools["github-cli"] != "" {
		t.Fatalf("legacy Candidate provenance = %#v, %v", candidateTools, err)
	}
	audit, err := db.WorkerAudit(ctx, deliveryClaim.RunID)
	if err != nil || audit.ImageDigest != oldImage || audit.LeaseGeneration != deliveryClaim.LeaseGeneration || audit.ContainerID != "delivery-container" {
		t.Fatalf("original Delivery Worker audit = %#v, %v", audit, err)
	}
}

func TestControllerRetriesFailedDeliveryAtAcceptedCandidateBoundaryWithActiveWorker(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}}, deliveryOutput: []byte("run:\n  status: completed\noutcome: failed\n")}
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "did not pass") {
		t.Fatalf("failed Delivery Controller error = %v", err)
	}
	candidate, err := db.CandidateRevision(ctx, claim.RunID)
	if err != nil {
		t.Fatalf("Candidate acceptance was not durable: %v", err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 0 {
		t.Fatalf("Delivery Controller should retry before Needs Attention: %#v, err = %v", questions, err)
	}
	if _, err := db.ClaimReviewRevision(ctx, version.ID, claim.TicketID, time.Minute, time.Now().UTC(), 1); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("review claim after failed delivery = %v, want not ready", err)
	}
	if _, err := db.ClaimPendingDeliveryClaims(ctx, "owner/repo", 1, time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingDeliveryClaims(ctx, "owner/repo", time.Now().UTC())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending delivery claims = %#v, %v", pending, err)
	}
	retryRunID := pending[0].RunID
	if _, _, pinned, err := db.DeliveryWorkerRuntime(ctx, pending[0]); err != nil || pinned {
		t.Fatalf("recovery Delivery Worker runtime pin = %v, %v; want Active selection", pinned, err)
	}
	if err := db.ActivateWorkerRelease(ctx, store.WorkerRelease{
		Version:        "0.2.0",
		SourceCommit:   "cccccccccccccccccccccccccccccccccccccccc",
		ImageReference: "ghcr.io/owner/worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ManifestJSON:   `{"schema_version":1,"codex_version":"2.0.0","github_cli_version":"2.98.0","go_version":"1.25.12","no_mistakes_version":"v2.0.0"}`,
		VerifiedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	retryRuntime := &fakeRuntime{}
	controller.Runtime = retryRuntime
	if err := controller.RetryDelivery(ctx, pending[0]); err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
	if len(retryRuntime.specs) != 1 || strings.Join(retryRuntime.specs[0].Command[:3], " ") != "no-mistakes axi run" {
		t.Fatalf("retry runtime specs = %#v", retryRuntime.specs)
	}
	if retryRuntime.specs[0].ImageDigest != "ghcr.io/owner/worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd" {
		t.Fatalf("retried delivery image = %q, want Active Worker image", retryRuntime.specs[0].ImageDigest)
	}
	if retryRuntime.specs[0].ToolVersions["codex"] != "2.0.0" || retryRuntime.specs[0].ToolVersions["github-cli"] != "2.98.0" || retryRuntime.specs[0].ToolVersions["no-mistakes"] != "v2.0.0" {
		t.Fatalf("retried delivery tools = %#v, want Active Worker tools", retryRuntime.specs[0].ToolVersions)
	}
	candidateImage, candidateTools, err := db.CandidateWorkerRuntime(ctx, version.ID, claim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if candidateImage != "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || candidateTools["codex"] != "1.0.0" {
		t.Fatalf("accepted Candidate runtime changed during delivery recovery: image=%q tools=%#v", candidateImage, candidateTools)
	}
	audit, err := db.WorkerAudit(ctx, retryRunID)
	if err != nil {
		t.Fatalf("retried Delivery Controller audit: %v", err)
	}
	var auditedTools map[string]string
	if err := json.Unmarshal([]byte(audit.ToolVersionsJSON), &auditedTools); err != nil {
		t.Fatalf("decode retried Delivery Controller tools audit: %v", err)
	}
	var auditedMounts []worker.Mount
	if err := json.Unmarshal([]byte(audit.MountsJSON), &auditedMounts); err != nil {
		t.Fatalf("decode retried Delivery Controller mounts audit: %v", err)
	}
	if audit.ContainerID != "delivery-container" || audit.ImageDigest != retryRuntime.specs[0].ImageDigest || audit.GitHubWriteCredentials || !strings.Contains(audit.ExtraHostsJSON, worker.GatewayHostMapping) || !reflect.DeepEqual(auditedTools, retryRuntime.specs[0].ToolVersions) || !reflect.DeepEqual(auditedMounts, retryRuntime.specs[0].Mounts) {
		t.Fatalf("retried Delivery Controller audit = %#v", audit)
	}
	session, err := db.TicketSession(ctx, version.ID, claim.TicketID)
	if err != nil {
		t.Fatal(err)
	}
	if session.AcceptedCandidateRunID != claim.RunID || session.AcceptedCommit != candidate.CommitSHA {
		t.Fatalf("accepted Candidate identity changed during recovery: session=%#v candidate=%#v", session, candidate)
	}
	t.Logf("accepted Candidate preserved: run=%s commit=%s image=%s tools=%v", claim.RunID, candidate.CommitSHA, candidateImage, candidateTools)
	t.Logf("recovery Delivery Worker launched: run=%s image=%s tools=%v mounts=%s extra_hosts=%s github_write_credentials=%t container=%s", retryRunID, audit.ImageDigest, auditedTools, audit.MountsJSON, audit.ExtraHostsJSON, audit.GitHubWriteCredentials, audit.ContainerID)
	assertWorkflowDeliveryEnvironment(t, retryRuntime.specs[0], claim.SessionID, claim.RunID, pending[0].RunID)
	pending, err = db.PendingDeliveryClaims(ctx, "owner/repo", time.Now().UTC())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending delivery claims after retry = %#v, %v", pending, err)
	}
}

func TestControllerRetriesPreContainerDeliveryInfrastructureFailure(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	runtime := &fakeRuntime{
		results:     []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}},
		deliveryErr: worker.InfrastructureError{Err: errors.New("Docker daemon unavailable")},
	}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "Docker daemon unavailable") {
		t.Fatalf("pre-container infrastructure error = %v", err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 0 {
		t.Fatalf("pre-container infrastructure failure escalated: questions=%#v, err=%v", questions, err)
	}
	earlyRetries, err := db.ClaimPendingDeliveryClaims(ctx, "owner/repo", 1, time.Minute, time.Now().UTC())
	if err != nil || len(earlyRetries) != 0 {
		t.Fatalf("pre-container infrastructure failure skipped backoff: retries=%#v, err=%v", earlyRetries, err)
	}
	retries, err := db.ClaimPendingDeliveryClaims(ctx, "owner/repo", 1, time.Minute, time.Now().UTC().Add(2*time.Minute))
	if err != nil || len(retries) != 1 || retries[0].SessionID != claim.SessionID {
		t.Fatalf("pre-container delivery retry = %#v, err=%v", retries, err)
	}
}

func TestControllerRejectsActiveWorkerManifestWithoutGitHubCLI(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, _, claim := createClaim(t, ctx, root)
	defer db.Close()
	if err := db.ActivateWorkerRelease(ctx, store.WorkerRelease{
		Version:        "0.2.0",
		SourceCommit:   "cccccccccccccccccccccccccccccccccccccccc",
		ImageReference: "ghcr.io/owner/worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ManifestJSON:   `{"schema_version":1,"codex_version":"2.0.0","go_version":"1.25.12","no_mistakes_version":"v2.0.0"}`,
		VerifiedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	controller := agent.Controller{
		Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")},
		Runtime: runtime, GatewayURL: "http://gateway.test",
	}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err == nil || !strings.Contains(err.Error(), "invalid release manifest") {
		t.Fatalf("missing GitHub CLI manifest error = %v", err)
	}
	if len(runtime.specs) != 0 {
		t.Fatalf("worker launched with incomplete tool provenance: %#v", runtime.specs)
	}
}

func TestControllerPausesHumanQualityGateAndRetriesItsExactAnswer(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	gateOutput := []byte("run:\n  id: delivery-1\n  status: waiting\noutcome: waiting-for-human\ngate:\n  id: gate-17\n  source: no-mistakes\n  finding_id: finding-42\n  action: ask_user\n  reason: choose the migration strategy\n  allowed_answers[2]: proceed, decline\n")
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}}, deliveryOutput: gateOutput}
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err != nil {
		t.Fatalf("run with human gate: %v", err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 1 {
		t.Fatalf("quality gate questions = %#v, err=%v", questions, err)
	}
	question := questions[0]
	if question.Kind != "quality_gate" || !strings.Contains(question.Prompt, "finding-42") || !strings.Contains(question.Prompt, "proceed, decline") || !strings.Contains(question.Prompt, "workflow-answer:"+question.ID) {
		t.Fatalf("quality gate question = %#v", question)
	}
	if _, err := db.ClaimReviewRevision(ctx, version.ID, claim.TicketID, time.Minute, time.Now().UTC(), 1); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("manual Agent continuation bypassed active gate: %v", err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, "owner/repo", question.ID, "not-allowed", time.Now().UTC()); !errors.Is(err, store.ErrInvalidClaim) {
		t.Fatalf("invalid quality gate answer = %v, want ErrInvalidClaim", err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, "owner/repo", question.ID, "proceed", time.Now().UTC()); err != nil {
		t.Fatalf("answer quality gate: %v", err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, "owner/repo", question.ID, "proceed", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("duplicate quality gate answer = %v, want ErrNotFound", err)
	}
	if _, err := db.ClaimPendingDeliveryClaims(ctx, "owner/repo", 1, time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingDeliveryClaims(ctx, "owner/repo", time.Now().UTC())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending gate retry = %#v, err=%v", pending, err)
	}
	retryRuntime := &fakeRuntime{}
	controller.Runtime = retryRuntime
	if err := controller.RetryDelivery(ctx, pending[0]); err != nil {
		t.Fatalf("retry answered gate: %v", err)
	}
	if len(retryRuntime.specs) != 1 || retryRuntime.specs[0].Environment["NO_MISTAKES_GATE_ID"] != "gate-17" || retryRuntime.specs[0].Environment["NO_MISTAKES_GATE_FINDING_ID"] != "finding-42" || retryRuntime.specs[0].Environment["NO_MISTAKES_GATE_ANSWER"] != "proceed" || retryRuntime.specs[0].Environment["NO_MISTAKES_GATE_ACTION"] != "ask-user" || retryRuntime.specs[0].Environment["NO_MISTAKES_GATE_ENFORCED"] != "true" {
		t.Fatalf("quality gate retry environment = %#v", retryRuntime.specs)
	}
	if strings.Contains(strings.Join(retryRuntime.specs[0].Command, " "), "--yes") {
		t.Fatalf("Delivery Controller used forbidden global approval: %#v", retryRuntime.specs[0].Command)
	}
}

func TestControllerRejectsAnswerForStaleQualityGate(t *testing.T) {
	ctx := context.Background()
	source := initRepository(t)
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	gateOutput := []byte("run:\n  status: waiting\noutcome: waiting-for-human\ngate:\n  id: gate-stale\n  action: ask-user\n  reason: choose a migration strategy\n  allowed_answers[1]: proceed\n")
	runtime := &fakeRuntime{results: []worker.Result{{Output: codexOutput("codex-session", "implemented"), ContainerID: "container-1"}}, deliveryOutput: gateOutput}
	controller := agent.Controller{Store: db, Workspace: agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}, Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
	if _, err := controller.Run(ctx, candidateRequest(claim, source, "ticket-1", "implement")); err != nil {
		t.Fatal(err)
	}
	questions, err := db.OpenWorkflowQuestions(ctx, "owner/repo", 10)
	if err != nil || len(questions) != 1 {
		t.Fatalf("quality gate questions = %#v, err=%v", questions, err)
	}
	if err := db.MarkTicketDelivered(ctx, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if err := db.AnswerWorkflowQuestion(ctx, "owner/repo", questions[0].ID, "proceed", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale quality gate answer = %v, want ErrNotFound", err)
	}
}

func assertWorkflowDeliveryEnvironment(t *testing.T, spec worker.Spec, deliveryCycle, revisionRound, correlationID string) {
	t.Helper()
	if spec.Environment["NO_MISTAKES_WORKFLOW_MODE"] != "true" || spec.Environment["NO_MISTAKES_DELIVERY_CYCLE"] != deliveryCycle || spec.Environment["NO_MISTAKES_REVISION_ROUND"] != revisionRound || spec.Environment["NO_MISTAKES_CORRELATION_ID"] != correlationID {
		t.Fatalf("Delivery Controller workflow correlation environment = %#v", spec.Environment)
	}
	if spec.Environment["NM_HOME"] != "/codex-state/no-mistakes" {
		t.Fatalf("Delivery Controller no-mistakes state = %q, want persistent Ticket state", spec.Environment["NM_HOME"])
	}
	if spec.Environment["NO_MISTAKES_DEFAULT_BRANCH"] != "main" {
		t.Fatalf("Delivery Controller default branch = %q, want trusted source default", spec.Environment["NO_MISTAKES_DEFAULT_BRANCH"])
	}
}

func assertDeliveryOriginMount(t *testing.T, runtime *fakeRuntime, spec worker.Spec, source string) {
	t.Helper()
	if _, exists := spec.Environment["GIT_CONFIG_COUNT"]; exists {
		t.Fatalf("Delivery Controller retained additive Git origin override = %#v", spec.Environment)
	}
	if runtime.deliveryOrigin != "/source-repository" {
		t.Fatalf("Delivery Controller workspace origin = %q", runtime.deliveryOrigin)
	}
	workspaceOrigin := exec.Command("git", "-C", spec.WorkspacePath, "remote", "get-url", "--all", "origin")
	if output, err := workspaceOrigin.Output(); err != nil || !strings.EqualFold(filepath.Clean(strings.TrimSpace(string(output))), filepath.Clean(source)) {
		t.Fatalf("restored Ticket Workspace origin = %q, %v", output, err)
	}
	for _, mount := range spec.Mounts {
		if mount.Target == "/source-repository" {
			if mount.Source == source || !mount.ReadOnly {
				t.Fatalf("Delivery Controller source mount = %#v, want credential-free snapshot distinct from %q", mount, source)
			}
			bare := exec.Command("git", "-C", mount.Source, "rev-parse", "--is-bare-repository")
			if output, err := bare.Output(); err != nil || strings.TrimSpace(string(output)) != "true" {
				t.Fatalf("Delivery Controller source snapshot is not bare: %q, %v", output, err)
			}
			remotes := exec.Command("git", "-C", mount.Source, "remote")
			if output, err := remotes.Output(); err != nil || strings.TrimSpace(string(output)) != "" {
				t.Fatalf("Delivery Controller source snapshot remotes = %q, %v", output, err)
			}
			credentials := exec.Command("git", "-C", mount.Source, "config", "--local", "--get-regexp", "credential")
			if output, err := credentials.CombinedOutput(); err == nil || len(output) != 0 {
				t.Fatalf("Delivery Controller source snapshot exposed credential config: %q, %v", output, err)
			}
			if _, err := os.Stat(filepath.Join(mount.Source, "untracked-credential.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Delivery Controller source snapshot exposed untracked credential file: %v", err)
			}
			return
		}
	}
	t.Fatalf("Delivery Controller source mount missing: %#v", spec.Mounts)
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
			controller := agent.Controller{Store: db, Workspace: manager, Runtime: test.runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
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
		Runtime: runtime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test",
	}
	_, err := controller.Run(ctx, agent.RunRequest{Claim: claim, SourceRepository: "https://user:token@github.com/owner/repo.git", Branch: "ticket-1", Prompt: "implement"})
	if err == nil || !strings.Contains(err.Error(), "absolute local path") {
		t.Fatalf("credential-bearing source error = %v", err)
	}
	if len(runtime.specs) != 0 {
		t.Fatal("worker started with a credential-bearing workspace source")
	}
	if err := db.ReserveWorkerLaunch(ctx, claim, store.WorkerAudit{RunID: claim.RunID, LeaseGeneration: claim.LeaseGeneration, ImageDigest: "sha256:image", ToolVersions: map[string]string{"codex": "1", "github-cli": "1", "go": "1", "no-mistakes": "1"}}, time.Now().UTC()); err != nil {
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
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: firstRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
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
	controller := agent.Controller{Store: db, Workspace: manager, Runtime: firstRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
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
	firstController := agent.Controller{Store: db, Workspace: manager, Runtime: firstRuntime, ImageDigest: "sha256:image-1", ToolVersions: map[string]string{"codex": "1.0.0"}, GatewayURL: "http://gateway.test"}
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

func TestWorkspaceManagerReclaimsOnlyClosedSessionAfterRetention(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	workspacePath := filepath.Join(manager.RootDir, claim.SessionID)
	statePath := filepath.Join(manager.CodexStateRoot, claim.SessionID)
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent-1", WorkspacePath: workspacePath, CodexStatePath: statePath, Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := manager.ReclaimClosed(ctx, db, time.Hour, time.Now().UTC()); err != nil || reclaimed != 0 {
		t.Fatalf("early reclaim = %d, %v", reclaimed, err)
	}
	if reclaimed, err := manager.ReclaimClosed(ctx, db, time.Hour, time.Now().UTC().Add(2*time.Hour)); err != nil || reclaimed != 1 {
		t.Fatalf("reclaim = %d, %v", reclaimed, err)
	}
	if _, err := os.Stat(workspacePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists: %v", err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Codex state still exists: %v", err)
	}
	if reclaimed, err := manager.ReclaimClosed(ctx, db, time.Hour, time.Now().UTC().Add(3*time.Hour)); err != nil || reclaimed != 0 {
		t.Fatalf("idempotent reclaim = %d, %v", reclaimed, err)
	}
}

func TestWorkspaceManagerReclaimsPreBindDeliverySnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	deliverySourcePath := filepath.Join(manager.RootDir, ".delivery-sources", claim.SessionID, ".delivery-orphan")
	if err := os.MkdirAll(deliverySourcePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := manager.ReclaimClosed(ctx, db, time.Hour, time.Now().UTC().Add(2*time.Hour)); err != nil || reclaimed != 1 {
		t.Fatalf("pre-bind reclaim = %d, %v", reclaimed, err)
	}
	if _, err := os.Stat(filepath.Join(manager.RootDir, ".delivery-sources", claim.SessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-bind Delivery Source still exists: %v", err)
	}
}

func TestWorkspaceCleanupFailureDoesNotRollbackDeliveredTicket(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, version, claim := createClaim(t, ctx, root)
	defer db.Close()
	manager := agent.WorkspaceManager{RootDir: filepath.Join(root, "workspaces"), CodexStateRoot: filepath.Join(root, "codex")}
	workspacePath := filepath.Join(manager.RootDir, claim.SessionID)
	outsideStatePath := filepath.Join(root, "outside", claim.SessionID)
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideStatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BindAgent(ctx, store.AgentBinding{SessionID: claim.SessionID, AgentIdentity: "agent-1", WorkspacePath: workspacePath, CodexStatePath: outsideStatePath, Branch: "ticket-1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTicketDelivered(ctx, version.ID, claim.TicketID); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := manager.ReclaimClosed(ctx, db, time.Hour, time.Now().UTC().Add(2*time.Hour)); err == nil || reclaimed != 0 {
		t.Fatalf("cleanup failure reclaim = %d, %v; want zero and an error", reclaimed, err)
	}
	projection, err := db.PlanProjection(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Tickets[0].State != "Delivered" {
		t.Fatalf("ticket state after cleanup failure = %q, want Delivered", projection.Tickets[0].State)
	}
	if _, err := db.CurrentClaim(ctx, version.ID, claim.TicketID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delivered current claim after cleanup failure = %v, want ErrNotFound", err)
	}
}

const candidateCommitPlaceholder = "WORKSPACE_HEAD_COMMIT"

func codexOutput(sessionID, summary string) []byte {
	return codexOutputWithCommit(sessionID, summary, candidateCommitPlaceholder)
}

func codexOutputWithCommit(sessionID, summary string, commit any) []byte {
	message, _ := json.Marshal(map[string]any{"summary": summary, "commit": commit, "checks": []map[string]string{{"command": "go test ./...", "outcome": "passed"}}, "plan_amendment": nil})
	item, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(message)}})
	return []byte(`{"type":"thread.started","thread_id":"` + sessionID + `"}` + "\n" + string(item))
}

func candidateRequest(claim store.TicketClaim, source, branch, prompt string) agent.RunRequest {
	return agent.RunRequest{Claim: claim, SourceRepository: source, Branch: branch, Prompt: prompt, Publication: store.CandidatePublication{Repository: "owner/repo", Branch: branch, ExpectRemoteAbsent: true, Title: claim.TicketTitle}}
}

func activateTestWorker(t *testing.T, ctx context.Context, db *store.Store) {
	t.Helper()
	if err := db.ActivateWorkerRelease(ctx, store.WorkerRelease{
		Version:        "0.1.0",
		SourceCommit:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ImageReference: "ghcr.io/owner/worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ManifestJSON:   `{"schema_version":1,"codex_version":"1.0.0","github_cli_version":"2.97.0","go_version":"1.25.12","no_mistakes_version":"v1.0.0"}`,
		VerifiedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
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

func looseObjects(t *testing.T, repository string) []string {
	t.Helper()
	objects := filepath.Join(repository, ".git", "objects")
	var relativeObjects []string
	err := filepath.WalkDir(objects, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != objects && (entry.Name() == "info" || entry.Name() == "pack") {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(objects, path)
		if err != nil {
			return err
		}
		relativeObjects = append(relativeObjects, relative)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(relativeObjects) == 0 {
		t.Fatal("source repository has no loose Git objects")
	}
	return relativeObjects
}
