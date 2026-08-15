package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const SourceOverrideEnvironment = "WORKFLOW_CODEX_AUTH_FILE"

// Resolver discovers the invoking Codex ChatGPT login through the redacted,
// machine-readable doctor report and confirms the active login status.
type Resolver struct {
	LookupEnvironment func(string) string
	Doctor            func(context.Context) ([]byte, error)
	LoginStatus       func(context.Context) ([]byte, error)
}

type doctorReport struct {
	SchemaVersion int                    `json:"schemaVersion"`
	CodexVersion  string                 `json:"codexVersion"`
	Checks        map[string]doctorCheck `json:"checks"`
}

type doctorCheck struct {
	Status  string            `json:"status"`
	Details map[string]string `json:"details"`
}

func ResolveChatGPT(ctx context.Context) (string, error) {
	return (Resolver{}).ResolveChatGPT(ctx)
}

func (r Resolver) ResolveChatGPT(ctx context.Context) (string, error) {
	lookup := r.LookupEnvironment
	if lookup == nil {
		lookup = os.Getenv
	}
	doctor := r.Doctor
	if doctor == nil {
		doctor = func(ctx context.Context) ([]byte, error) {
			output, err := exec.CommandContext(ctx, "codex", "doctor", "--json").CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("query redacted Codex doctor report: %w", err)
			}
			return output, nil
		}
	}
	doctorOutput, err := doctor(ctx)
	if err != nil {
		return "", err
	}
	discovered, err := authenticationSourceFromDoctor(doctorOutput)
	if err != nil {
		return "", err
	}
	status := r.LoginStatus
	if status == nil {
		status = func(ctx context.Context) ([]byte, error) {
			output, err := exec.CommandContext(ctx, "codex", "login", "status").CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("query Codex login status: %w", err)
			}
			return output, nil
		}
	}
	output, err := status(ctx)
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToLower(string(output)), "logged in using chatgpt") {
		return "", errors.New("Codex must be logged in using ChatGPT")
	}

	source := strings.TrimSpace(lookup(SourceOverrideEnvironment))
	if source == "" {
		source = discovered
	}
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("%s must be an absolute path supplied by the invoking Codex integration", SourceOverrideEnvironment)
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", fmt.Errorf("resolve Codex authentication source: %w", err)
	}
	if err := ValidateChatGPT(source); err != nil {
		return "", err
	}
	return filepath.Clean(source), nil
}

func authenticationSourceFromDoctor(raw []byte) (string, error) {
	var report doctorReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return "", fmt.Errorf("decode redacted Codex doctor report: %w", err)
	}
	if report.SchemaVersion != 1 || strings.TrimSpace(report.CodexVersion) == "" {
		return "", errors.New("Codex doctor machine-readable capability is unsupported")
	}
	auth, authOK := report.Checks["auth.credentials"]
	config, configOK := report.Checks["config.load"]
	if !authOK || !configOK || auth.Status != "ok" || config.Status != "ok" {
		return "", errors.New("Codex doctor did not verify authentication and configuration")
	}
	if auth.Details["stored ChatGPT tokens"] != "true" || !strings.EqualFold(auth.Details["stored auth mode"], "chatgpt") {
		return "", errors.New("Codex doctor did not verify stored ChatGPT authentication")
	}
	source := strings.TrimSpace(auth.Details["auth file"])
	home := strings.TrimSpace(config.Details["CODEX_HOME"])
	if !filepath.IsAbs(source) || !filepath.IsAbs(home) || !strings.EqualFold(filepath.Clean(filepath.Dir(source)), filepath.Clean(home)) {
		return "", errors.New("Codex doctor returned an invalid authentication source boundary")
	}
	return filepath.Clean(source), nil
}
