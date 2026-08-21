package setupcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/skyhuang233/workflow/internal/repositorycontract"
	"github.com/skyhuang233/workflow/internal/setupeffect"
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	windowsLocalPath  = regexp.MustCompile(`^[A-Za-z]:[\\/].+`)
	githubRepository  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

var preconditionPlanKinds = map[string]PlanKind{
	"platform_release": "",
	"git_head":         RepositoryOnboarding, "onboarding_snapshot": RepositoryOnboarding, "github_policy": RepositoryOnboarding, "github_default_head": RepositoryOnboarding,
}

var expectedResultPlanKinds = map[string]PlanKind{"repository_admission": RepositoryOnboarding}

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
	if p.Kind == RepositoryOnboarding {
		if err := validateRepositoryOnboardingIdentity(p); err != nil {
			return err
		}
		created := map[string]bool{}
		effectIndexes := map[string]int{}
		for index, effect := range p.Effects {
			effectIndexes[effect.ID] = index
			if effect.Kind == "create_repository" {
				created[effect.Subject] = true
			}
			if effect.Kind == "publish_history" && effect.Parameters["new_repository"] == "true" && !created[effect.Subject] {
				return fmt.Errorf("publish history effect %q claims a new repository without an earlier approved creation", effect.ID)
			}
			if effect.Kind == "repository_contract_pr" && effect.Parameters["base_head"] == "" {
				baselineID := effect.Parameters["base_head_effect_id"]
				baselineIndex, found := effectIndexes[baselineID]
				if !found || baselineIndex >= index {
					return fmt.Errorf("repository contract effect %q lacks an earlier Initial Repository Baseline binding", effect.ID)
				}
				baseline := p.Effects[baselineIndex]
				if baseline.Kind != "initial_baseline" || baseline.Parameters["repository"] != effect.Subject {
					return fmt.Errorf("repository contract effect %q has an invalid Initial Repository Baseline binding", effect.ID)
				}
			}
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

func validateRepositoryOnboardingIdentity(plan Plan) error {
	target := plan.Target.GitHubRepository
	if !githubRepository.MatchString(target) {
		return errors.New("Repository Onboarding requires one exact GitHub repository target")
	}
	canonicalURL := "https://github.com/" + target + ".git"
	for _, precondition := range plan.Preconditions {
		if (precondition.Kind == "github_policy" || precondition.Kind == "github_default_head") && !strings.EqualFold(precondition.Subject, target) {
			return fmt.Errorf("precondition %q targets a different GitHub repository", precondition.ID)
		}
	}
	for _, effect := range plan.Effects {
		var repository string
		switch effect.Kind {
		case "create_repository", "publish_history", "repository_features", "repository_contract_pr", "repository_admission":
			repository = effect.Subject
		case "github_label":
			repository, _, _ = strings.Cut(effect.Subject, "#")
		case "initial_baseline":
			if !strings.EqualFold(effect.Subject, plan.Target.RepositoryPath) {
				return fmt.Errorf("effect %q targets a different local repository", effect.ID)
			}
			repository = effect.Parameters["repository"]
		case "local_fast_forward":
			if !strings.EqualFold(effect.Subject, plan.Target.RepositoryPath) {
				return fmt.Errorf("effect %q targets a different local repository", effect.ID)
			}
			repository = effect.Parameters["repository"]
		default:
			return fmt.Errorf("effect %q is not a Repository Onboarding effect", effect.ID)
		}
		if !strings.EqualFold(repository, target) {
			return fmt.Errorf("effect %q targets a different GitHub repository", effect.ID)
		}
		if effect.Kind == "create_repository" && !strings.EqualFold(effect.Parameters["owner"]+"/"+effect.Parameters["name"], target) {
			return fmt.Errorf("effect %q creates a different GitHub repository", effect.ID)
		}
		if (effect.Kind == "initial_baseline" || effect.Kind == "repository_contract_pr") && !strings.EqualFold(effect.Parameters["source_url"], canonicalURL) {
			return fmt.Errorf("effect %q uses a source URL outside the GitHub target", effect.ID)
		}
	}
	for _, expected := range plan.ExpectedResults {
		if !strings.EqualFold(expected.Subject, target) {
			return fmt.Errorf("expected result %q targets a different GitHub repository", expected.ID)
		}
	}
	return nil
}

func validateExpectedResultBinding(plan Plan, expected ExpectedResult) error {
	switch expected.Kind {
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
}

func validateEffect(planKind PlanKind, effect Effect) error {
	schema, ok := setupeffect.Lookup(effect.Kind)
	if !ok {
		return fmt.Errorf("unknown effect kind %q", effect.Kind)
	}
	if string(schema.PlanKind) != string(planKind) {
		return fmt.Errorf("effect kind %q is unsupported for %q", effect.Kind, planKind)
	}
	if !containsSemantic(schema.Actions, effect.Action) {
		return fmt.Errorf("action %q is unsupported for effect kind %q", effect.Action, effect.Kind)
	}
	allowed := map[string]bool{}
	for _, key := range append(append([]string{}, schema.Required...), schema.Optional...) {
		allowed[key] = true
	}
	for key := range effect.Parameters {
		if !allowed[key] {
			return fmt.Errorf("unknown parameter %q", key)
		}
	}
	for _, key := range schema.Required {
		value, exists := effect.Parameters[key]
		allowEmpty := (effect.Kind == "repository_contract_pr" && key == "base_head") || (effect.Kind == "local_fast_forward" && key == "pre_merge_head")
		if !exists || (!allowEmpty && strings.TrimSpace(value) == "") {
			return fmt.Errorf("parameter %q is required", key)
		}
	}
	for _, key := range []string{"manifest_digest"} {
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
	if effect.Kind == "create_repository" {
		approvedRepository := effect.Parameters["owner"] + "/" + effect.Parameters["name"]
		if effect.Parameters["approval_absent_repository"] != approvedRepository || effect.Subject != approvedRepository {
			return errors.New("repository creation is not bound to its exact approval-time absence identity")
		}
	}
	if effect.Kind == "repository_contract_pr" && effect.Parameters["base_head"] != "" && effect.Parameters["base_head_effect_id"] != "" {
		return errors.New("repository contract base must use either an approved HEAD or Initial Repository Baseline evidence")
	}
	if effect.Kind == "repository_features" {
		switch effect.Parameters["allowed_actions"] {
		case "all", "local_only", "selected":
		default:
			return errors.New("Actions policy is invalid")
		}
	}
	for _, key := range []string{"files_json", "bootstrap_files_json", "before_files_json", "required_checks_json", "labels_json"} {
		if value, exists := effect.Parameters[key]; exists && !json.Valid([]byte(value)) {
			return fmt.Errorf("parameter %q must be valid JSON", key)
		}
	}
	if effect.Kind == "repository_contract_pr" {
		var checks []struct {
			Context string `json:"context"`
			AppID   int64  `json:"app_id"`
		}
		if err := json.Unmarshal([]byte(effect.Parameters["required_checks_json"]), &checks); err != nil {
			return errors.New("repository contract required checks are invalid")
		}
		identities := map[string]int64{}
		for _, check := range checks {
			if check.Context == "" || check.AppID <= 0 {
				return errors.New("repository contract required check lacks an App identity")
			}
			if existing := identities[check.Context]; existing != 0 && existing != check.AppID {
				return errors.New("repository contract required check context has conflicting App identities")
			}
			identities[check.Context] = check.AppID
		}
		if identities[repositorycontract.RequiredCheckName] != repositorycontract.GitHubActionsAppID {
			return errors.New("workflow-contract required check has an unapproved App identity")
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
	schema, ok := setupeffect.Lookup(effect.Kind)
	if !ok {
		return fmt.Errorf("unknown effect kind %q", effect.Kind)
	}
	return validateEffect(PlanKind(schema.PlanKind), effect)
}

func ValidatePreconditionForExecution(precondition Precondition) error {
	if _, ok := preconditionPlanKinds[precondition.Kind]; !ok {
		return fmt.Errorf("unknown precondition kind %q", precondition.Kind)
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
