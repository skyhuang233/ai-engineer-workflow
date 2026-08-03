// Package doctor verifies the concrete host and external contracts required by
// the single-host workflow control plane.
package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const Redacted = "[REDACTED]"

type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
)

type ToolPin struct {
	Version string `json:"version"`
}

type NoMistakesPin struct {
	Version            string `json:"version"`
	UpstreamRepository string `json:"upstream_repository"`
	UpstreamCommit     string `json:"upstream_commit"`
	ForkRepository     string `json:"fork_repository"`
	ForkRelease        string `json:"fork_release"`
	LinuxAMD64SHA256   string `json:"linux_amd64_sha256"`
}

type WorkerPin struct {
	Image        string `json:"image"`
	LocalBuildID string `json:"local_build_id"`
}

type RuntimePolicy struct {
	MaxWorkerAttempts int `json:"max_worker_attempts"`
}

type GitHubPin struct {
	TestRepository      string              `json:"test_repository"`
	DefaultBranch       string              `json:"default_branch"`
	RequiredCheck       string              `json:"required_check"`
	RequiredReviewCount int                 `json:"required_review_count"`
	Credential          GitHubCredentialPin `json:"credential"`
}

type GitHubCredentialPin struct {
	Kind                string            `json:"kind"`
	AllowedRepositories []string          `json:"allowed_repositories"`
	Permissions         map[string]string `json:"permissions"`
	ApprovedBy          string            `json:"approved_by"`
	ApprovedAt          string            `json:"approved_at"`
	FingerprintSHA256   string            `json:"fingerprint_sha256"`
}

type UpgradePolicy struct {
	Rule string `json:"rule"`
}

type Config struct {
	SchemaVersion int           `json:"schema_version"`
	Codex         ToolPin       `json:"codex"`
	NoMistakes    NoMistakesPin `json:"no_mistakes"`
	Worker        WorkerPin     `json:"worker"`
	Runtime       RuntimePolicy `json:"runtime"`
	GitHub        GitHubPin     `json:"github"`
	Upgrade       UpgradePolicy `json:"upgrade"`
}

var (
	shaPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	imagePattern  = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode toolchain config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	switch {
	case c.SchemaVersion != 1:
		return fmt.Errorf("unsupported toolchain schema version %d", c.SchemaVersion)
	case strings.TrimSpace(c.Codex.Version) == "":
		return errors.New("Codex version is required")
	case strings.TrimSpace(c.NoMistakes.Version) == "":
		return errors.New("no-mistakes version is required")
	case !repoPattern.MatchString(c.NoMistakes.UpstreamRepository):
		return errors.New("no-mistakes upstream repository must be owner/name")
	case !shaPattern.MatchString(c.NoMistakes.UpstreamCommit):
		return errors.New("no-mistakes upstream commit must be a full SHA")
	case !repoPattern.MatchString(c.NoMistakes.ForkRepository):
		return errors.New("no-mistakes fork repository must be owner/name")
	case strings.TrimSpace(c.NoMistakes.ForkRelease) == "":
		return errors.New("no-mistakes fork release is required")
	case !sha256Pattern.MatchString(c.NoMistakes.LinuxAMD64SHA256):
		return errors.New("no-mistakes Linux asset checksum must be SHA-256")
	case !imagePattern.MatchString(c.Worker.Image):
		return errors.New("worker image must use an immutable sha256 digest")
	case !strings.HasPrefix(c.Worker.LocalBuildID, "sha256:") || !sha256Pattern.MatchString(strings.TrimPrefix(c.Worker.LocalBuildID, "sha256:")):
		return errors.New("worker local build ID must be SHA-256")
	case c.Runtime.MaxWorkerAttempts < 1:
		return errors.New("runtime max worker attempts must be positive")
	case !repoPattern.MatchString(c.GitHub.TestRepository):
		return errors.New("GitHub test repository must be owner/name")
	case strings.TrimSpace(c.GitHub.DefaultBranch) == "":
		return errors.New("GitHub default branch is required")
	case strings.TrimSpace(c.GitHub.RequiredCheck) == "":
		return errors.New("GitHub required check is required")
	case c.GitHub.RequiredReviewCount < 1:
		return errors.New("at least one human review is required")
	case c.GitHub.Credential.Kind != "fine-grained-pat" && c.GitHub.Credential.Kind != "github-app":
		return errors.New("GitHub credential must be a fine-grained PAT or GitHub App")
	case len(c.GitHub.Credential.AllowedRepositories) == 0:
		return errors.New("GitHub credential repository allowlist is required")
	case len(c.GitHub.Credential.Permissions) == 0:
		return errors.New("GitHub credential permission declaration is required")
	case strings.TrimSpace(c.Upgrade.Rule) == "":
		return errors.New("toolchain upgrade rule is required")
	default:
		return nil
	}
}

type Result struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`
}

type Check interface {
	Name() string
	Run(context.Context) Result
}

type Runner struct {
	Checks  []Check
	Secrets []string
	Now     func() time.Time
}

type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Results     []Result  `json:"results"`
}

func (r Runner) Run(ctx context.Context) Report {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	results := make([]Result, 0, len(r.Checks))
	for _, check := range r.Checks {
		result := check.Run(ctx)
		result.Name = check.Name()
		result.Summary = redact(result.Summary, r.Secrets)
		results = append(results, result)
	}
	return Report{GeneratedAt: now, Results: results}
}

func (r Report) Passed() bool {
	if len(r.Results) == 0 {
		return false
	}
	for _, result := range r.Results {
		if result.Status != Pass {
			return false
		}
	}
	return true
}

func (r Report) Markdown() string {
	var output strings.Builder
	output.WriteString("# Workflow doctor report\n\n")
	output.WriteString("Generated: ")
	output.WriteString(r.GeneratedAt.Format(time.RFC3339))
	output.WriteString("\n\n")
	output.WriteString("| Check | Status | Evidence |\n")
	output.WriteString("|---|---|---|\n")
	for _, result := range r.Results {
		fmt.Fprintf(&output, "| %s | %s | %s |\n", escapeTable(result.Name), result.Status, escapeTable(result.Summary))
	}
	return output.String()
}

func redact(value string, secrets []string) string {
	ordered := append([]string(nil), secrets...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, secret := range ordered {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, Redacted)
		}
	}
	return value
}

func escapeTable(value string) string {
	value = strings.ReplaceAll(value, "|", `\|`)
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.ReplaceAll(value, "\n", " ")
}

type Executor interface {
	Run(context.Context, []string) ([]byte, error)
}

type OSExecutor struct{}

func (OSExecutor) Run(ctx context.Context, command []string) ([]byte, error) {
	if len(command) == 0 {
		return nil, errors.New("command is empty")
	}
	return exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
}

type CommandExpectation struct {
	Command      []string
	Contains     []string
	Tool         string
	ExactVersion string
	ExactCommit  string
}

type CommandCheck struct {
	CheckName    string
	Executor     Executor
	Version      CommandExpectation
	Capabilities []CommandExpectation
}

type CodexResumeCheck struct {
	Executor Executor
	Nonce    string
}

func (CodexResumeCheck) Name() string { return "Codex session resume" }

func (c CodexResumeCheck) Run(ctx context.Context) Result {
	if c.Executor == nil {
		return Result{Status: Fail, Summary: "command executor is missing"}
	}
	workdir, err := os.MkdirTemp("", "workflow-codex-resume-*")
	if err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	defer os.RemoveAll(workdir)
	nonce := c.Nonce
	if nonce == "" {
		value, err := randomToken()
		if err != nil {
			return Result{Status: Fail, Summary: err.Error()}
		}
		nonce = value
	}
	initial, err := c.Executor.Run(ctx, []string{"codex", "exec", "--skip-git-repo-check", "--json", "--sandbox", "read-only", "-C", workdir, "Remember this nonce for the next turn: " + nonce + ". Reply with exactly: phase-one"})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("create Codex session: %v", err)}
	}
	sessionID := jsonEventString(initial, "thread.started", "thread_id")
	if sessionID == "" {
		return Result{Status: Fail, Summary: "Codex did not emit a persistent session ID"}
	}
	resumed, err := c.Executor.Run(ctx, []string{"codex", "exec", "resume", "--json", "--skip-git-repo-check", sessionID, "Reply with only the nonce I gave you in the previous turn."})
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("resume Codex session: %v", err)}
	}
	if !strings.Contains(string(resumed), nonce) {
		return Result{Status: Fail, Summary: "resumed Codex session did not recall prior-turn context"}
	}
	return Result{Status: Pass, Summary: "persistent session ID created and resumed successfully"}
}

func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return fmt.Sprintf("%x", sum)
}

func jsonEventString(output []byte, eventType, field string) string {
	for _, line := range strings.Split(string(output), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["type"] == eventType {
			if value, ok := event[field].(string); ok {
				return value
			}
		}
	}
	return ""
}

func (c CommandCheck) Name() string { return c.CheckName }

func (c CommandCheck) Run(ctx context.Context) Result {
	if c.Executor == nil {
		return Result{Status: Fail, Summary: "command executor is missing"}
	}
	output, err := c.Executor.Run(ctx, c.Version.Command)
	trimmed := strings.TrimSpace(string(output))
	if err != nil {
		return Result{Status: Fail, Summary: fmt.Sprintf("%s: %v (%s)", strings.Join(c.Version.Command, " "), err, trimmed)}
	}
	version, commit, err := parseCommandVersion(c.Version.Tool, trimmed)
	if err != nil {
		return Result{Status: Fail, Summary: err.Error()}
	}
	if version != c.Version.ExactVersion || commit != c.Version.ExactCommit {
		return Result{Status: Fail, Summary: fmt.Sprintf("%s reports version %q commit %q, want %q %q", c.Version.Tool, version, commit, c.Version.ExactVersion, c.Version.ExactCommit)}
	}
	for _, expectation := range c.Capabilities {
		output, err := c.Executor.Run(ctx, expectation.Command)
		trimmed := strings.TrimSpace(string(output))
		if err != nil {
			return Result{Status: Fail, Summary: fmt.Sprintf("%s: %v (%s)", strings.Join(expectation.Command, " "), err, trimmed)}
		}
		for _, required := range expectation.Contains {
			if !strings.Contains(trimmed, required) {
				return Result{Status: Fail, Summary: fmt.Sprintf("%s does not report required capability %q", strings.Join(expectation.Command, " "), required)}
			}
		}
	}
	return Result{Status: Pass, Summary: trimmed}
}

var (
	codexVersionPattern      = regexp.MustCompile(`^codex-cli[[:space:]]+([^[:space:]]+)$`)
	noMistakesVersionPattern = regexp.MustCompile(`^no-mistakes version[[:space:]]+([^[:space:]]+)[[:space:]]+\(([0-9a-f]{7,40})\)([[:space:]].*)?$`)
)

func parseCommandVersion(tool, output string) (string, string, error) {
	var matches []string
	switch tool {
	case "codex":
		matches = codexVersionPattern.FindStringSubmatch(output)
	case "no-mistakes":
		matches = noMistakesVersionPattern.FindStringSubmatch(output)
	default:
		return "", "", fmt.Errorf("unsupported version parser %q", tool)
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("%s emitted an unrecognized version string %q", tool, output)
	}
	commit := ""
	if len(matches) > 2 {
		commit = matches[2]
	}
	return matches[1], commit, nil
}
