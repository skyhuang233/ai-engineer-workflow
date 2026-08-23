package doctor

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

type authenticationFailureError struct{}

func (authenticationFailureError) Error() string               { return "authentication failed" }
func (authenticationFailureError) AuthenticationFailure() bool { return true }

func TestConfigRequiresImmutableProductionPins(t *testing.T) {
	config := validConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"codex version", func(c *Config) { c.Codex.Version = "" }},
		{"GitHub CLI version", func(c *Config) { c.GitHubCLI.Version = "" }},
		{"GitHub CLI checksum", func(c *Config) { c.GitHubCLI.LinuxAMD64SHA256 = "latest" }},
		{"no-mistakes repository", func(c *Config) { c.NoMistakes.Repository = "main" }},
		{"no-mistakes commit", func(c *Config) { c.NoMistakes.Commit = "main" }},
		{"worker image repository", func(c *Config) { c.Worker.ImageRepository = "latest" }},
		{"release repository", func(c *Config) { c.Worker.ReleaseRepository = "" }},
		{"release repository owner", func(c *Config) { c.Worker.ReleaseRepository = "collaborator/workflow" }},
		{"required check", func(c *Config) { c.GitHub.RequiredCheck = "" }},
		{"workflow path", func(c *Config) { c.GitHub.WorkflowPath = "workflow-contract.yml" }},
		{"GitHub credential kind", func(c *Config) { c.GitHub.Credential.Kind = "fine-grained-pat" }},
		{"PAT scopes", func(c *Config) { c.GitHub.Credential.RequiredScopes = []string{"repo"} }},
		{"PAT fixed relative path", func(c *Config) { c.GitHub.Credential.PlaintextRelativePath = `credentials\github.pat` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := validConfig()
			tt.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("Validate() accepted an incomplete or floating production pin")
			}
		})
	}
}

func TestRunnerProducesDeterministicRedactedReport(t *testing.T) {
	runner := Runner{
		Now: func() time.Time { return time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC) },
		Checks: []Check{
			checkFunc{name: "codex", result: Result{Status: Pass, Summary: "codex 0.146.0"}},
			checkFunc{name: "github", result: Result{Status: Fail, Summary: "token=super-secret rejected"}},
		},
		Secrets: []string{"super-secret"},
	}
	report := runner.Run(context.Background())
	if report.Passed() {
		t.Fatal("Passed() = true with a failed check")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "super-secret") {
		t.Fatalf("JSON report leaked a secret: %s", encoded)
	}
	if !strings.Contains(string(encoded), Redacted) {
		t.Fatalf("JSON report did not record redaction: %s", encoded)
	}
	markdown := report.Markdown()
	if strings.Contains(markdown, "super-secret") || !strings.Contains(markdown, "FAIL") {
		t.Fatalf("unsafe or incomplete Markdown report:\n%s", markdown)
	}
}

func TestReportReturnsTypedAuthenticationFailure(t *testing.T) {
	failure := authenticationFailureError{}
	report := Report{Results: []Result{{Status: Fail, Err: failure}}}
	if !errors.Is(report.AuthenticationFailure(), failure) {
		t.Fatalf("AuthenticationFailure() = %v, want %v", report.AuthenticationFailure(), failure)
	}
	if (Report{Results: []Result{{Status: Fail, Err: errors.New("network unavailable")}}}).AuthenticationFailure() != nil {
		t.Fatal("AuthenticationFailure() returned a transient failure")
	}
}

func TestCommandCheckMatchesPinnedVersionAndCapabilities(t *testing.T) {
	executor := fakeExecutor{
		outputs: map[string]string{
			"codex --version":   "codex-cli 0.146.0",
			"codex exec --help": "resume --json --output-schema --ephemeral",
		},
	}
	check := CommandCheck{
		CheckName: "codex",
		Executor:  executor,
		Version: CommandExpectation{
			Command:      []string{"codex", "--version"},
			Tool:         "codex",
			ExactVersion: "0.146.0",
		},
		Capabilities: []CommandExpectation{{
			Command:  []string{"codex", "exec", "--help"},
			Contains: []string{"resume", "--json", "--output-schema", "--ephemeral"},
		}},
	}
	result := check.Run(context.Background())
	if result.Status != Pass {
		t.Fatalf("Run() = %#v, want PASS", result)
	}

	executor.outputs["codex --version"] = "codex-cli 0.145.0"
	check.Executor = executor
	result = check.Run(context.Background())
	if result.Status != Fail {
		t.Fatalf("Run() = %#v, want FAIL for version drift", result)
	}
}

func TestCommandCheckFailsClosedWithoutExecutable(t *testing.T) {
	check := CommandCheck{
		CheckName: "no-mistakes",
		Executor:  fakeExecutor{err: errors.New("executable not found")},
		Version: CommandExpectation{
			Command:      []string{"no-mistakes", "--version"},
			Tool:         "no-mistakes",
			ExactVersion: "v1.41.2",
			ExactCommit:  "867d64d9c2df89f3f204ad1f5528e5bf7b460caa",
		},
	}
	if result := check.Run(context.Background()); result.Status != Fail {
		t.Fatalf("Run() = %#v, want FAIL", result)
	}
}

func TestCommandCheckMatchesEmbeddedNoMistakesCommit(t *testing.T) {
	commit := "867d64d9c2df89f3f204ad1f5528e5bf7b460caa"
	check := CommandCheck{
		CheckName: "no-mistakes",
		Executor: fakeExecutor{outputs: map[string]string{
			"no-mistakes --version": "no-mistakes version v1.41.2 (867d64d) 2026-07-24T06:16:02Z",
		}},
		Version:        CommandExpectation{Command: []string{"no-mistakes", "--version"}, Tool: "no-mistakes", ExactVersion: "v1.41.2", ExactCommit: commit},
		BuildInfo:      fakeNoMistakesBuildInfo(commit),
		ExecutablePath: "no-mistakes",
	}
	if result := check.Run(context.Background()); result.Status != Pass {
		t.Fatalf("Run() = %#v, want exact-commit success", result)
	}
}

func TestCommandCheckRejectsNoMistakesCommitPrefixCollision(t *testing.T) {
	commit := "867d64d9c2df89f3f204ad1f5528e5bf7b460caa"
	collision := "867d64d9c2df89f3f204ad1f5528e5bf7b460cab"
	check := CommandCheck{
		CheckName: "no-mistakes",
		Executor: fakeExecutor{outputs: map[string]string{
			"no-mistakes --version": "no-mistakes version v1.41.2 (867d64d) 2026-07-24T06:16:02Z",
		}},
		Version:        CommandExpectation{Command: []string{"no-mistakes", "--version"}, Tool: "no-mistakes", ExactVersion: "v1.41.2", ExactCommit: commit},
		BuildInfo:      fakeNoMistakesBuildInfo(collision),
		ExecutablePath: "no-mistakes",
	}
	if result := check.Run(context.Background()); result.Status != Fail {
		t.Fatalf("Run() = %#v, want commit mismatch failure", result)
	}
}

func TestCommandCheckRejectsModifiedNoMistakesBuild(t *testing.T) {
	commit := "867d64d9c2df89f3f204ad1f5528e5bf7b460caa"
	check := CommandCheck{
		CheckName: "no-mistakes",
		Executor: fakeExecutor{outputs: map[string]string{
			"no-mistakes --version": "no-mistakes version v1.41.2 (867d64d) 2026-07-24T06:16:02Z",
		}},
		Version:        CommandExpectation{Command: []string{"no-mistakes", "--version"}, Tool: "no-mistakes", ExactVersion: "v1.41.2", ExactCommit: commit},
		BuildInfo:      fakeNoMistakesBuildInfoWithModified(commit, "true"),
		ExecutablePath: "no-mistakes",
	}
	if result := check.Run(context.Background()); result.Status != Fail {
		t.Fatalf("Run() = %#v, want modified-build failure", result)
	}
}

func fakeNoMistakesBuildInfo(commit string) func(string) (*buildinfo.BuildInfo, error) {
	return fakeNoMistakesBuildInfoWithModified(commit, "false")
}

func fakeNoMistakesBuildInfoWithModified(commit, modified string) func(string) (*buildinfo.BuildInfo, error) {
	return func(string) (*buildinfo.BuildInfo, error) {
		return &buildinfo.BuildInfo{
			Path: "github.com/kunchenguid/no-mistakes/cmd/no-mistakes",
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: commit},
				{Key: "vcs.modified", Value: modified},
			},
		}, nil
	}
}

func validConfig() Config {
	return Config{
		SchemaVersion: 7,
		Codex:         ToolPin{Version: "0.148.0"},
		GitHubCLI:     GitHubCLIPin{Version: "2.97.0", LinuxAMD64SHA256: "a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112"},
		Go:            GoPin{Version: "1.26.6", LinuxAMD64SHA256: "708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89"},
		NoMistakes: NoMistakesPin{
			Version:    "v1.41.2",
			Repository: "skyhuang233/no-mistakes",
			Commit:     "eafc10e0fc7306be3af1750524aa2067e5048942",
		},
		Worker: WorkerPin{
			ImageRepository:   "ghcr.io/skyhuang233/workflow-worker",
			ReleaseRepository: "skyhuang233/ai-engineer-workflow",
		},
		Runtime: RuntimePolicy{MaxWorkerAttempts: 3},
		GitHub: GitHubPin{
			TestRepository: "skyhuang233/workflow-integration-test",
			DefaultBranch:  "develop",
			RequiredCheck:  "workflow-contract",
			WorkflowPath:   ".github/workflows/workflow-contract.yml",
			Credential: GitHubCredentialPin{
				Kind:                  "classic-pat",
				Owner:                 "skyhuang233",
				RequiredScopes:        []string{"repo", "workflow"},
				PlaintextRelativePath: `state\credentials\github.pat`,
			},
		},
		Upgrade: UpgradePolicy{Rule: "Upgrade only after compatibility and integration tests pass."},
	}
}

func TestConfigAcceptsClassicPATSchemaSeven(t *testing.T) {
	config := validConfig()
	config.GitHub.TestRepository = ""
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.GitHub.Credential.RequiredScopes = []string{"repo"}
	if err := config.Validate(); err == nil {
		t.Fatal("scope-deficient PAT config accepted")
	}
}

func TestCodexResumeCheckUsesReturnedSessionID(t *testing.T) {
	executor := &recordingExecutor{outputs: [][]byte{
		[]byte("{\"type\":\"thread.started\",\"thread_id\":\"session-7\"}\n"),
		[]byte("{\"type\":\"item.completed\",\"item\":{\"text\":\"nonce-7\"}}\n"),
	}}
	result := (CodexResumeCheck{Executor: executor, Nonce: "nonce-7"}).Run(context.Background())
	if result.Status != Pass {
		t.Fatalf("Run() = %#v, want PASS", result)
	}
	if got := strings.Join(executor.commands[1], " "); !strings.Contains(got, "resume") || !strings.Contains(got, "session-7") {
		t.Fatalf("resume command = %q", got)
	}
}

func TestWorkerCodexSessionCheckAuthenticatesAndResumesInPinnedImage(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{outputs: [][]byte{
		[]byte("{\"type\":\"thread.started\",\"thread_id\":\"worker-session-7\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"summary\\\":\\\"phase-one\\\",\\\"commit\\\":null,\\\"checks\\\":[{\\\"command\\\":\\\"doctor schema probe\\\",\\\"outcome\\\":\\\"passed\\\"}],\\\"plan_amendment\\\":null}\"}}\n"),
		[]byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"summary\\\":\\\"worker-nonce-7\\\",\\\"commit\\\":null,\\\"checks\\\":[{\\\"command\\\":\\\"doctor schema probe\\\",\\\"outcome\\\":\\\"passed\\\"}],\\\"plan_amendment\\\":null}\"}}\n"),
	}}
	result := (WorkerCodexSessionCheck{
		Executor: executor, Image: "ghcr.io/owner/worker@sha256:bbbb", AuthFile: authFile, Nonce: "worker-nonce-7",
	}).Run(context.Background())
	if result.Status != Pass {
		t.Fatalf("Run() = %#v, want PASS", result)
	}
	if len(executor.commands) != 4 {
		t.Fatalf("commands = %#v, want initial/resume containers and explicit cleanup", executor.commands)
	}
	initial := strings.Join(executor.commands[0], " ")
	resumed := strings.Join(executor.commands[2], " ")
	if !strings.HasPrefix(initial, "docker run --name") || strings.Contains(initial, " --rm ") || !strings.Contains(initial, "CODEX_HOME=/codex-state") || !strings.Contains(initial, "ghcr.io/owner/worker@sha256:bbbb codex exec --sandbox read-only") {
		t.Fatalf("initial Worker Codex command = %q", initial)
	}
	for _, command := range []string{initial, resumed} {
		if !strings.Contains(command, "--name workflow-doctor-codex-") || !strings.Contains(command, "--label com.skyhuang233.workflow.setup-probe=true") {
			t.Fatalf("Worker Codex command omits unique ownership metadata: %q", command)
		}
		if !strings.Contains(command, "--cap-add SYS_ADMIN") || !strings.Contains(command, "--security-opt seccomp=unconfined") {
			t.Fatalf("Worker Codex command omits bubblewrap sandbox permissions: %q", command)
		}
		if !strings.Contains(command, "--output-schema /codex-state/output-schema.json") {
			t.Fatalf("Worker Codex command omits the Candidate output schema: %q", command)
		}
	}
	for _, index := range []int{1, 3} {
		if got := strings.Join(executor.commands[index], " "); !strings.HasPrefix(got, "docker rm -f workflow-doctor-codex-") {
			t.Fatalf("cleanup command = %q", got)
		}
	}
	if !strings.Contains(resumed, "codex exec --sandbox read-only resume") || !strings.Contains(resumed, "worker-session-7") {
		t.Fatalf("resumed Worker Codex command = %q", resumed)
	}
	t.Logf("Doctor result: %s\ncreate: %s\nresume: %s", result.Summary, initial, resumed)
}

func TestWorkerCodexSessionCheckTracksDeterministicCleanupResources(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{outputs: [][]byte{
		workerCodexTestOutput("worker-session-7", strictCandidate("phase-one")),
		workerCodexTestOutput("", strictCandidate("worker-nonce-7")),
	}}
	var begun, completed []string
	result := (WorkerCodexSessionCheck{
		Executor: executor, Image: "sha256:image", AuthFile: authFile, Nonce: "worker-nonce-7", ProbeID: "abcdef123456",
		BeginCleanup:    func(kind, id, resource string) error { begun = append(begun, kind+"|"+id+"|"+resource); return nil },
		CompleteCleanup: func(id string) error { completed = append(completed, id); return nil },
	}).Run(context.Background())
	if result.Status != Pass {
		t.Fatalf("result=%#v", result)
	}
	commands := strings.Join([]string{strings.Join(executor.commands[0], " "), strings.Join(executor.commands[2], " ")}, "\n")
	for _, want := range []string{"workflow-doctor-codex-abcdef123456-initial", "workflow-doctor-codex-abcdef123456-resume", "setup-probe-id=abcdef123456"} {
		if !strings.Contains(commands, want) {
			t.Fatalf("commands lack %q: %s", want, commands)
		}
	}
	if len(begun) != 3 || len(completed) != 3 {
		t.Fatalf("begun=%#v completed=%#v", begun, completed)
	}
}

func TestWorkerCodexSessionCheckFailsWhenTemporaryAuthCleanupFails(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{outputs: [][]byte{
		workerCodexTestOutput("worker-session-7", strictCandidate("phase-one")),
		workerCodexTestOutput("", strictCandidate("worker-nonce-7")),
	}}
	result := (WorkerCodexSessionCheck{
		Executor: executor, Image: "sha256:image", AuthFile: authFile, Nonce: "worker-nonce-7",
		RemoveAll: func(string) error { return errors.New("cleanup denied") },
	}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "cleanup") || !strings.Contains(result.Summary, "cleanup denied") {
		t.Fatalf("cleanup failure result = %#v", result)
	}
}

func TestWorkerCodexSessionCheckAggregatesPrimaryAndCleanupFailures(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := (WorkerCodexSessionCheck{
		Executor:  &recordingExecutor{outputs: [][]byte{[]byte("bad response")}},
		Image:     "sha256:image",
		AuthFile:  authFile,
		RemoveAll: func(string) error { return errors.New("cleanup denied") },
	}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "create schema probe") || !strings.Contains(result.Summary, "cleanup denied") {
		t.Fatalf("combined failure result = %#v", result)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "create schema probe") || !strings.Contains(result.Err.Error(), "cleanup denied") {
		t.Fatalf("combined failure error = %v", result.Err)
	}
}

func TestWorkerCodexSessionCheckRejectsInvalidStructuredResponse(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{outputs: [][]byte{
		[]byte("{\"type\":\"thread.started\",\"thread_id\":\"worker-session-7\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{}\"}}\n"),
	}}
	result := (WorkerCodexSessionCheck{Executor: executor, Image: "sha256:image", AuthFile: authFile}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "invalid structured response") {
		t.Fatalf("Run() = %#v, want invalid structured response failure", result)
	}
}

func TestWorkerCodexSessionCheckRequiresStrictRootFields(t *testing.T) {
	for _, tt := range []struct {
		name      string
		candidate string
	}{
		{name: "missing commit", candidate: `{"summary":"phase-one","checks":[],"plan_amendment":null}`},
		{name: "missing plan amendment", candidate: `{"summary":"phase-one","commit":null,"checks":[]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			authFile := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
				t.Fatal(err)
			}
			executor := &recordingExecutor{outputs: [][]byte{workerCodexTestOutput("worker-session-7", tt.candidate)}}
			result := (WorkerCodexSessionCheck{Executor: executor, Image: "sha256:image", AuthFile: authFile}).Run(context.Background())
			if result.Status != Fail || !strings.Contains(result.Summary, "create schema probe") {
				t.Fatalf("Run() = %#v, want strict create schema failure", result)
			}
		})
	}
}

func TestWorkerCodexSessionCheckUsesFinalValidAgentMessageForNonce(t *testing.T) {
	for _, tt := range []struct {
		name      string
		responses []string
		want      Status
	}{
		{name: "final response recalls nonce", responses: []string{"wrong", "worker-nonce-7"}, want: Pass},
		{name: "final response violates nonce", responses: []string{"worker-nonce-7", "wrong"}, want: Fail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			authFile := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
				t.Fatal(err)
			}
			resumed := make([]string, 0, len(tt.responses))
			for _, summary := range tt.responses {
				resumed = append(resumed, strictCandidate(summary))
			}
			executor := &recordingExecutor{outputs: [][]byte{
				workerCodexTestOutput("worker-session-7", strictCandidate("phase-one")),
				workerCodexTestOutput("", resumed...),
			}}
			result := (WorkerCodexSessionCheck{Executor: executor, Image: "sha256:image", AuthFile: authFile, Nonce: "worker-nonce-7"}).Run(context.Background())
			if result.Status != tt.want {
				t.Fatalf("Run() = %#v, want %s", result, tt.want)
			}
		})
	}
}

func TestWorkerCodexSessionCheckReportsRejectedAuthenticationWithoutLeakingOutput(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authFile, doctorTestChatGPTAuth("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &recordingExecutor{
		outputs: [][]byte{[]byte("401 Unauthorized: Missing bearer or basic authentication in header secret-output")},
		errs:    []error{errors.New("exit status 1")},
	}
	result := (WorkerCodexSessionCheck{Executor: executor, Image: "sha256:image", AuthFile: authFile}).Run(context.Background())
	if result.Status != Fail || !strings.Contains(result.Summary, "authentication was rejected") {
		t.Fatalf("Run() = %#v, want redacted authentication failure", result)
	}
	if strings.Contains(result.Summary, "secret-output") {
		t.Fatalf("authentication report leaked Worker output: %q", result.Summary)
	}
}

func doctorTestChatGPTAuth(accessToken string) []byte {
	return []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":"account","id_token":"id-token","refresh_token":"refresh-token"}}`, accessToken))
}

func strictCandidate(summary string) string {
	encoded, _ := json.Marshal(map[string]any{
		"summary": summary, "commit": nil, "checks": []map[string]string{{"command": "doctor schema probe", "outcome": "passed"}}, "plan_amendment": nil,
	})
	return string(encoded)
}

func workerCodexTestOutput(sessionID string, candidates ...string) []byte {
	var output strings.Builder
	if sessionID != "" {
		event, _ := json.Marshal(map[string]string{"type": "thread.started", "thread_id": sessionID})
		output.Write(event)
		output.WriteByte('\n')
	}
	for _, candidate := range candidates {
		event, _ := json.Marshal(map[string]any{
			"type": "item.completed",
			"item": map[string]string{"type": "agent_message", "text": candidate},
		})
		output.Write(event)
		output.WriteByte('\n')
	}
	return []byte(output.String())
}

type checkFunc struct {
	name   string
	result Result
}

func (c checkFunc) Name() string               { return c.name }
func (c checkFunc) Run(context.Context) Result { return c.result }

type fakeExecutor struct {
	outputs map[string]string
	err     error
}

type recordingExecutor struct {
	outputs  [][]byte
	errs     []error
	commands [][]string
}

func (e *recordingExecutor) Run(_ context.Context, command []string) ([]byte, error) {
	e.commands = append(e.commands, append([]string(nil), command...))
	if len(command) >= 3 && command[0] == "docker" && command[1] == "rm" && command[2] == "-f" {
		return nil, nil
	}
	output := e.outputs[0]
	e.outputs = e.outputs[1:]
	var err error
	if len(e.errs) > 0 {
		err = e.errs[0]
		e.errs = e.errs[1:]
	}
	return output, err
}

func (f fakeExecutor) Run(_ context.Context, command []string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.outputs[strings.Join(command, " ")]), nil
}
