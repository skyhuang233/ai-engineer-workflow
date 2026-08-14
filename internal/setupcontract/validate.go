package setupcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	windowsLocalPath  = regexp.MustCompile(`^[A-Za-z]:[\\/].+`)
	githubRepository  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

func ParsePlan(raw []byte) (Plan, []byte, string, error) {
	canonical, digest, err := Canonicalize(raw)
	if err != nil {
		return Plan{}, nil, "", err
	}
	var plan Plan
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return Plan{}, nil, "", fmt.Errorf("decode Setup Plan: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Plan{}, nil, "", err
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, nil, "", err
	}
	return plan, canonical, digest, nil
}

func (p Plan) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported Setup Plan schema version %d", p.SchemaVersion)
	}
	if !identifierPattern.MatchString(p.PlanID) {
		return errors.New("Setup Plan ID is required and must be stable")
	}
	switch p.Kind {
	case PlatformBootstrap:
		if !isAbsoluteLocalWindowsPath(p.Target.WorkflowHome) {
			return errors.New("Platform Bootstrap target Workflow Home must be an absolute local Windows path")
		}
		if p.Target.RepositoryPath != "" || p.Target.GitHubRepository != "" {
			return errors.New("Platform Bootstrap target must not claim a repository identity")
		}
	case RepositoryOnboarding:
		if !isAbsoluteLocalWindowsPath(p.Target.RepositoryPath) {
			return errors.New("Repository Onboarding target path must be an absolute local Windows path")
		}
		if !isAbsoluteLocalWindowsPath(p.Target.WorkflowHome) {
			return errors.New("Repository Onboarding Workflow Home must be an absolute local Windows path")
		}
		if p.Target.GitHubRepository != "" && !githubRepository.MatchString(p.Target.GitHubRepository) {
			return errors.New("Repository Onboarding GitHub target must be an owner/name")
		}
	default:
		return fmt.Errorf("unknown Setup Plan kind %q", p.Kind)
	}
	if len(p.Preconditions) == 0 || len(p.Effects) == 0 || len(p.ExpectedResults) == 0 {
		return errors.New("Setup Plan requires preconditions, effects, and expected results")
	}
	if err := validateStableIDs(p.Preconditions, func(v Precondition) string { return v.ID }, "precondition"); err != nil {
		return err
	}
	if err := validateStableIDs(p.Effects, func(v Effect) string { return v.ID }, "effect"); err != nil {
		return err
	}
	if err := validateStableIDs(p.ExpectedResults, func(v ExpectedResult) string { return v.ID }, "expected result"); err != nil {
		return err
	}
	for _, precondition := range p.Preconditions {
		if strings.TrimSpace(precondition.Kind) == "" || strings.TrimSpace(precondition.Subject) == "" || strings.TrimSpace(precondition.Expected) == "" {
			return fmt.Errorf("precondition %q is incomplete", precondition.ID)
		}
	}
	for _, effect := range p.Effects {
		if strings.TrimSpace(effect.Kind) == "" || strings.TrimSpace(effect.Subject) == "" || strings.TrimSpace(effect.Action) == "" || effect.Parameters == nil {
			return fmt.Errorf("effect %q is incomplete", effect.ID)
		}
	}
	for _, expected := range p.ExpectedResults {
		if strings.TrimSpace(expected.Kind) == "" || strings.TrimSpace(expected.Subject) == "" || strings.TrimSpace(expected.Expected) == "" {
			return fmt.Errorf("expected result %q is incomplete", expected.ID)
		}
	}
	encoded, _ := json.Marshal(p)
	var semantic any
	_ = json.Unmarshal(encoded, &semantic)
	if path, found := findSecret(semantic, "$"); found {
		return fmt.Errorf("Setup Plan contains forbidden secret-shaped content at %s", path)
	}
	return nil
}

func (r ExecutionResult) Validate() error {
	if r.SchemaVersion != SchemaVersion || !identifierPattern.MatchString(r.PlanID) || !identifierPattern.MatchString(r.AttemptID) || !sha256Pattern.MatchString(r.PlanDigest) {
		return errors.New("Setup Execution Result identity is invalid")
	}
	if r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) {
		return errors.New("Setup Execution Result timestamps are invalid")
	}
	switch r.Status {
	case ExecutionSucceeded, ExecutionIncomplete, ExecutionDrifted, ExecutionBlocked:
	default:
		return errors.New("Setup Execution Result status is invalid")
	}
	seen := make(map[string]struct{}, len(r.Effects))
	for _, effect := range r.Effects {
		if !identifierPattern.MatchString(effect.EffectID) {
			return errors.New("Setup Execution Result effect identity is invalid")
		}
		if _, duplicate := seen[effect.EffectID]; duplicate {
			return fmt.Errorf("duplicate effect result %q", effect.EffectID)
		}
		seen[effect.EffectID] = struct{}{}
		switch effect.Status {
		case EffectSatisfied, EffectRequired, EffectConflicting, EffectFailed:
		default:
			return fmt.Errorf("effect %q has invalid status", effect.EffectID)
		}
	}
	return nil
}

func validateStableIDs[T any](values []T, id func(T) string, kind string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		identifier := id(value)
		if !identifierPattern.MatchString(identifier) {
			return fmt.Errorf("%s ID %q is invalid", kind, identifier)
		}
		if _, duplicate := seen[identifier]; duplicate {
			return fmt.Errorf("duplicate %s ID %q", kind, identifier)
		}
		seen[identifier] = struct{}{}
	}
	return nil
}

func isAbsoluteLocalWindowsPath(value string) bool {
	return windowsLocalPath.MatchString(value) && !strings.HasPrefix(value, `\\`)
}

func findSecret(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
			for _, forbidden := range []string{"token", "pat_body", "password", "private_key", "privatekey", "secret"} {
				if strings.Contains(normalized, forbidden) {
					return path + "." + key, true
				}
			}
			if foundPath, found := findSecret(child, path+"."+key); found {
				return foundPath, true
			}
		}
	case []any:
		for index, child := range typed {
			if foundPath, found := findSecret(child, fmt.Sprintf("%s[%d]", path, index)); found {
				return foundPath, true
			}
		}
	case string:
		lower := strings.ToLower(strings.TrimSpace(typed))
		if strings.HasPrefix(lower, "ghp_") || strings.HasPrefix(lower, "github_pat_") || strings.Contains(lower, "-----begin private key-----") {
			return path, true
		}
	}
	return "", false
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("JSON document contains a trailing value")
		}
		return err
	}
	return nil
}
