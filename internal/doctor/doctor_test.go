package doctor

import (
	"context"
	"encoding/json"
	"errors"
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
		{"upstream commit", func(c *Config) { c.NoMistakes.UpstreamCommit = "main" }},
		{"fork release", func(c *Config) { c.NoMistakes.ForkRelease = "" }},
		{"asset checksum", func(c *Config) { c.NoMistakes.LinuxAMD64SHA256 = "latest" }},
		{"worker version", func(c *Config) { c.Worker.Version = "" }},
		{"worker image repository", func(c *Config) { c.Worker.ImageRepository = "latest" }},
		{"release repository", func(c *Config) { c.Worker.ReleaseRepository = "" }},
		{"integration repository", func(c *Config) { c.GitHub.TestRepository = "" }},
		{"required check", func(c *Config) { c.GitHub.RequiredCheck = "" }},
		{"workflow path", func(c *Config) { c.GitHub.WorkflowPath = "workflow-contract.yml" }},
		{"all repositories credential", func(c *Config) { c.GitHub.Credential.AllRepositories = false }},
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

func TestCommandCheckRejectsCommitPrefixCollision(t *testing.T) {
	check := CommandCheck{
		CheckName: "no-mistakes",
		Executor: fakeExecutor{outputs: map[string]string{
			"no-mistakes --version": "no-mistakes version v1.41.2 (867d64d9c2df89f3f204ad1f5528e5bf7b460cab) 2026-07-24T06:16:02Z",
		}},
		Version: CommandExpectation{Command: []string{"no-mistakes", "--version"}, Tool: "no-mistakes", ExactVersion: "v1.41.2", ExactCommit: "867d64d9c2df89f3f204ad1f5528e5bf7b460caa"},
	}
	if result := check.Run(context.Background()); result.Status != Fail {
		t.Fatalf("Run() = %#v, want exact-commit failure", result)
	}
}

func TestNoMistakesVersionParserRequiresFullCommit(t *testing.T) {
	if _, _, err := parseCommandVersion("no-mistakes", "no-mistakes version v1.41.2 (867d64d) 2026-07-24T06:16:02Z"); err == nil {
		t.Fatal("parseCommandVersion accepted an abbreviated no-mistakes commit")
	}
}

func validConfig() Config {
	return Config{
		SchemaVersion: 2,
		Codex:         ToolPin{Version: "0.146.0"},
		NoMistakes: NoMistakesPin{
			Version:            "v1.41.2",
			UpstreamRepository: "kunchenguid/no-mistakes",
			UpstreamCommit:     "867d64d9c2df89f3f204ad1f5528e5bf7b460caa",
			ForkRepository:     "skyhuang233/no-mistakes",
			ForkRelease:        "workflow-v1.41.2.0",
			LinuxAMD64SHA256:   "a100c58bdfe7df9f598ecec32553d5fbd8eb0079912fc830f362011fd9dc8825",
		},
		Worker: WorkerPin{
			Version:           "0.1.0",
			ImageRepository:   "ghcr.io/skyhuang233/workflow-worker",
			ReleaseRepository: "skyhuang233/workflow",
		},
		Runtime: RuntimePolicy{MaxWorkerAttempts: 3},
		GitHub: GitHubPin{
			TestRepository: "skyhuang233/workflow-integration-test",
			DefaultBranch:  "main",
			RequiredCheck:  "workflow-contract",
			WorkflowPath:   ".github/workflows/workflow-contract.yml",
			Credential: GitHubCredentialPin{
				Kind:            "fine-grained-pat",
				Owner:           "skyhuang233",
				AllRepositories: true,
				Permissions: map[string]string{
					"actions": "read", "contents": "write", "issues": "write",
					"metadata": "read", "pull_requests": "write",
				},
			},
		},
		Upgrade: UpgradePolicy{Rule: "Upgrade only after compatibility and integration tests pass."},
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
	commands [][]string
}

func (e *recordingExecutor) Run(_ context.Context, command []string) ([]byte, error) {
	e.commands = append(e.commands, append([]string(nil), command...))
	output := e.outputs[0]
	e.outputs = e.outputs[1:]
	return output, nil
}

func (f fakeExecutor) Run(_ context.Context, command []string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.outputs[strings.Join(command, " ")]), nil
}
