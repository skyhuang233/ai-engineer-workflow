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

type effectContract struct {
	planKind PlanKind
	actions  []string
	required []string
	optional []string
}

var effectContracts = map[string]effectContract{
	"platform_cli":           {PlatformBootstrap, []string{"install"}, []string{"version", "sha256"}, nil},
	"workflow_skill_bundle":  {PlatformBootstrap, []string{"install"}, []string{"version", "managed_skills_json", "files_json"}, nil},
	"docker_desktop":         {PlatformBootstrap, []string{"install", "upgrade", "repair"}, []string{"version", "installer_url", "windows_amd64_sha256"}, nil},
	"github_pat":             {PlatformBootstrap, []string{"persist", "replace"}, []string{"input", "owner"}, []string{"api_base"}},
	"platform_installation":  {PlatformBootstrap, []string{"record"}, []string{"version", "release_manifest_digest", "platform_setup_contract_json", "platform_setup_contract_digest", "workflow_cli_sha256"}, nil},
	"control_plane":          {PlatformBootstrap, []string{"start", "replace"}, []string{"version", "release_manifest_digest", "platform_setup_contract_digest", "workflow_cli_sha256"}, nil},
	"create_repository":      {RepositoryOnboarding, []string{"create"}, []string{"owner", "authenticated_login", "name", "private"}, nil},
	"initial_baseline":       {RepositoryOnboarding, []string{"commit_and_push"}, []string{"branch", "files_json", "repository", "source_url"}, nil},
	"publish_history":        {RepositoryOnboarding, []string{"push"}, []string{"branch", "head"}, nil},
	"github_label":           {RepositoryOnboarding, []string{"reconcile"}, []string{"name", "color", "description"}, nil},
	"repository_features":    {RepositoryOnboarding, []string{"enable"}, []string{"issues", "actions", "allowed_actions"}, nil},
	"repository_contract_pr": {RepositoryOnboarding, []string{"create_check_merge"}, []string{"base_branch", "base_head", "source_url", "before_files_json", "files_json", "manifest_digest", "required_checks_json"}, nil},
	"repository_admission":   {RepositoryOnboarding, []string{"verify_and_record"}, []string{"default_branch", "manifest_digest", "contract_version"}, []string{"labels_json", "actions_allowed"}},
	"local_fast_forward":     {RepositoryOnboarding, []string{"fast_forward_if_safe"}, []string{"repository", "branch", "pre_merge_head", "merge_head_effect_id"}, nil},
}

var preconditionPlanKinds = map[string]PlanKind{
	"host_identity": PlatformBootstrap, "platform_release": "", "platform_setup_contract": PlatformBootstrap,
	"git_head": RepositoryOnboarding, "github_policy": RepositoryOnboarding, "github_default_head": RepositoryOnboarding,
}

var expectedResultPlanKinds = map[string]PlanKind{
	"platform_readiness":   PlatformBootstrap,
	"repository_admission": RepositoryOnboarding,
}

// VerifyExpectedResults executes every accepted expected-result semantic after
// effect readback, so a result kind cannot be a decorative success claim.
func VerifyExpectedResults(plan Plan, effects []EffectResult) error {
	statuses := make(map[string]EffectStatus, len(effects))
	for _, effect := range effects {
		statuses[effect.EffectID] = effect.Status
	}
	for _, effect := range plan.Effects {
		if statuses[effect.ID] != EffectSatisfied {
			return fmt.Errorf("effect %q did not satisfy an expected Setup result", effect.ID)
		}
	}
	for _, expected := range plan.ExpectedResults {
		switch expected.Kind {
		case "platform_readiness":
			if expected.Expected != "ready" {
				return fmt.Errorf("platform readiness expected result %q is invalid", expected.ID)
			}
		case "repository_admission":
			matched := false
			for _, effect := range plan.Effects {
				if effect.Kind == "repository_admission" && effect.Subject == expected.Subject && effect.Parameters["manifest_digest"] == expected.Expected {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("repository admission expected result %q is not bound to its verification effect", expected.ID)
			}
		default:
			return fmt.Errorf("unknown expected result kind %q", expected.Kind)
		}
	}
	return nil
}

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
		allowEmptyExpected := precondition.Kind == "git_head"
		if strings.TrimSpace(precondition.Kind) == "" || strings.TrimSpace(precondition.Subject) == "" || (!allowEmptyExpected && strings.TrimSpace(precondition.Expected) == "") {
			return fmt.Errorf("precondition %q is incomplete", precondition.ID)
		}
		allowed, ok := preconditionPlanKinds[precondition.Kind]
		if !ok || allowed != "" && allowed != p.Kind {
			return fmt.Errorf("precondition %q kind %q is unsupported for %q", precondition.ID, precondition.Kind, p.Kind)
		}
	}
	for _, effect := range p.Effects {
		if strings.TrimSpace(effect.Kind) == "" || strings.TrimSpace(effect.Subject) == "" || strings.TrimSpace(effect.Action) == "" || effect.Parameters == nil {
			return fmt.Errorf("effect %q is incomplete", effect.ID)
		}
		if err := validateEffect(p.Kind, effect); err != nil {
			return fmt.Errorf("effect %q: %w", effect.ID, err)
		}
	}
	if p.Kind == PlatformBootstrap {
		if err := validatePlatformEffectPins(p.Effects); err != nil {
			return err
		}
	}
	for _, expected := range p.ExpectedResults {
		if strings.TrimSpace(expected.Kind) == "" || strings.TrimSpace(expected.Subject) == "" || strings.TrimSpace(expected.Expected) == "" {
			return fmt.Errorf("expected result %q is incomplete", expected.ID)
		}
		allowed, ok := expectedResultPlanKinds[expected.Kind]
		if !ok || allowed != p.Kind {
			return fmt.Errorf("expected result %q kind %q is unsupported for %q", expected.ID, expected.Kind, p.Kind)
		}
		if err := validateExpectedResultBinding(p, expected); err != nil {
			return err
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

func validateExpectedResultBinding(plan Plan, expected ExpectedResult) error {
	switch expected.Kind {
	case "platform_readiness":
		if expected.Expected != "ready" || expected.Subject != plan.Target.WorkflowHome {
			return fmt.Errorf("platform readiness expected result %q is not bound to its target", expected.ID)
		}
	case "repository_admission":
		for _, effect := range plan.Effects {
			if effect.Kind == "repository_admission" && effect.Subject == expected.Subject && effect.Parameters["manifest_digest"] == expected.Expected {
				return nil
			}
		}
		return fmt.Errorf("repository admission expected result %q is not bound to its verification effect", expected.ID)
	default:
		return fmt.Errorf("unknown expected result kind %q", expected.Kind)
	}
	return nil
}

func validateEffect(planKind PlanKind, effect Effect) error {
	schema, ok := effectContracts[effect.Kind]
	if !ok {
		return fmt.Errorf("unknown effect kind %q", effect.Kind)
	}
	if schema.planKind != planKind {
		return fmt.Errorf("effect kind %q is unsupported for %q", effect.Kind, planKind)
	}
	if !containsSemantic(schema.actions, effect.Action) {
		return fmt.Errorf("action %q is unsupported for effect kind %q", effect.Action, effect.Kind)
	}
	if planKind == PlatformBootstrap && isPlatformMutationEffect(effect.Kind) && effect.Kind != "platform_installation" && effect.Kind != "control_plane" {
		schema.required = append(schema.required, "release_manifest_digest", "platform_setup_contract_digest", "workflow_cli_sha256")
	}
	allowed := map[string]bool{}
	for _, key := range append(append([]string{}, schema.required...), schema.optional...) {
		allowed[key] = true
	}
	for key := range effect.Parameters {
		if !allowed[key] {
			return fmt.Errorf("unknown parameter %q", key)
		}
	}
	for _, key := range schema.required {
		value, exists := effect.Parameters[key]
		allowEmpty := (effect.Kind == "repository_contract_pr" && key == "base_head") || (effect.Kind == "local_fast_forward" && key == "pre_merge_head")
		if !exists || (!allowEmpty && strings.TrimSpace(value) == "") {
			return fmt.Errorf("parameter %q is required", key)
		}
	}
	for _, key := range []string{"sha256", "windows_amd64_sha256", "release_manifest_digest", "platform_setup_contract_digest", "workflow_cli_sha256", "manifest_digest"} {
		if value, exists := effect.Parameters[key]; exists && !sha256Pattern.MatchString(value) {
			return fmt.Errorf("parameter %q must be a lowercase SHA-256", key)
		}
	}
	for _, key := range []string{"head", "base_head", "pre_merge_head"} {
		if value, exists := effect.Parameters[key]; exists && value != "" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(value) {
			return fmt.Errorf("parameter %q must be a Git commit", key)
		}
	}
	for _, key := range []string{"private", "issues", "actions"} {
		if value, exists := effect.Parameters[key]; exists && value != "true" && value != "false" {
			return fmt.Errorf("parameter %q must be true or false", key)
		}
	}
	if effect.Kind == "github_pat" && effect.Parameters["input"] != "stdin" {
		return errors.New("GitHub PAT input must be stdin")
	}
	if effect.Kind == "docker_desktop" && !strings.HasPrefix(effect.Parameters["installer_url"], "https://") {
		return errors.New("Docker installer URL must use HTTPS")
	}
	if effect.Kind == "repository_features" {
		switch effect.Parameters["allowed_actions"] {
		case "all", "local_only", "selected":
		default:
			return errors.New("Actions policy is invalid")
		}
	}
	for _, key := range []string{"managed_skills_json", "files_json", "before_files_json", "required_checks_json", "labels_json", "platform_setup_contract_json"} {
		if value, exists := effect.Parameters[key]; exists && !json.Valid([]byte(value)) {
			return fmt.Errorf("parameter %q must be valid JSON", key)
		}
	}
	return nil
}

func containsSemantic(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// ValidateEffectForExecution keeps direct Readback and Apply calls on the same
// kind/action registry used by Setup Plan validation.
func ValidateEffectForExecution(effect Effect) error {
	schema, ok := effectContracts[effect.Kind]
	if !ok {
		return fmt.Errorf("unknown effect kind %q", effect.Kind)
	}
	return validateEffect(schema.planKind, effect)
}

func ValidatePreconditionForExecution(precondition Precondition) error {
	if _, ok := preconditionPlanKinds[precondition.Kind]; !ok {
		return fmt.Errorf("unknown precondition kind %q", precondition.Kind)
	}
	return nil
}

func isPlatformMutationEffect(kind string) bool {
	switch kind {
	case "platform_cli", "workflow_skill_bundle", "docker_desktop", "github_pat", "platform_installation", "control_plane":
		return true
	default:
		return false
	}
}

func validatePlatformEffectPins(effects []Effect) error {
	var releaseDigest, contractDigest, cliDigest string
	found := false
	for _, effect := range effects {
		if !isPlatformMutationEffect(effect.Kind) {
			continue
		}
		pins := []string{effect.Parameters["release_manifest_digest"], effect.Parameters["platform_setup_contract_digest"], effect.Parameters["workflow_cli_sha256"]}
		if !found {
			releaseDigest, contractDigest, cliDigest = pins[0], pins[1], pins[2]
			found = true
		} else if pins[0] != releaseDigest || pins[1] != contractDigest || pins[2] != cliDigest {
			return errors.New("Platform Bootstrap effects have inconsistent release pins")
		}
		if effect.Kind == "platform_cli" && effect.Parameters["sha256"] != pins[2] {
			return errors.New("Platform CLI checksum does not match the pinned Workflow CLI checksum")
		}
	}
	if !found {
		return errors.New("Platform Bootstrap plan has no release-bound platform effect")
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
