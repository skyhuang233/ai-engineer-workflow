// Package setupeffect owns the bounded vocabulary and execution contract for
// every Setup effect kind. Both Setup Plan validation and execution dispatch
// consume this registry so adding a kind cannot update only one side.
package setupeffect

type PlanKind string

const RepositoryOnboarding PlanKind = "repository_onboarding"

type EngineSemantics string

const (
	StandardEffect  EngineSemantics = "standard"
	AdmissionEffect EngineSemantics = "repository_admission"
)

type Descriptor struct {
	Kind     string
	PlanKind PlanKind
	Actions  []string
	Required []string
	Optional []string
	Engine   EngineSemantics
}

var descriptors = []Descriptor{
	{Kind: "create_repository", PlanKind: RepositoryOnboarding, Actions: []string{"create"}, Required: []string{"owner", "authenticated_login", "name", "private", "approval_absent_repository"}, Engine: StandardEffect},
	{Kind: "initial_baseline", PlanKind: RepositoryOnboarding, Actions: []string{"commit_and_push"}, Required: []string{"branch", "files_json", "repository", "source_url"}, Engine: StandardEffect},
	{Kind: "publish_history", PlanKind: RepositoryOnboarding, Actions: []string{"push"}, Required: []string{"branch", "head"}, Optional: []string{"new_repository"}, Engine: StandardEffect},
	{Kind: "github_label", PlanKind: RepositoryOnboarding, Actions: []string{"reconcile"}, Required: []string{"name", "color", "description"}, Engine: StandardEffect},
	{Kind: "repository_features", PlanKind: RepositoryOnboarding, Actions: []string{"enable"}, Required: []string{"issues", "actions", "allowed_actions"}, Engine: StandardEffect},
	{Kind: "repository_variable", PlanKind: RepositoryOnboarding, Actions: []string{"reconcile"}, Required: []string{"name", "value"}, Engine: StandardEffect},
	{Kind: "repository_contract_pr", PlanKind: RepositoryOnboarding, Actions: []string{"create_check_merge"}, Required: []string{"base_branch", "base_head", "source_url", "before_files_json", "files_json", "manifest_digest", "required_checks_json", "merge_method"}, Optional: []string{"base_head_effect_id"}, Engine: StandardEffect},
	{Kind: "repository_admission", PlanKind: RepositoryOnboarding, Actions: []string{"verify_and_record"}, Required: []string{"default_branch", "manifest_digest", "contract_version"}, Optional: []string{"labels_json", "actions_allowed"}, Engine: AdmissionEffect},
	{Kind: "local_fast_forward", PlanKind: RepositoryOnboarding, Actions: []string{"fast_forward_if_safe"}, Required: []string{"repository", "branch", "pre_merge_head", "merge_head_effect_id"}, Optional: []string{"pre_merge_head_effect_id"}, Engine: StandardEffect},
}

var byKind = func() map[string]Descriptor {
	result := make(map[string]Descriptor, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Kind == "" || descriptor.PlanKind == "" || len(descriptor.Actions) == 0 || descriptor.Engine == "" {
			panic("incomplete Setup effect descriptor")
		}
		if _, duplicate := result[descriptor.Kind]; duplicate {
			panic("duplicate Setup effect descriptor: " + descriptor.Kind)
		}
		result[descriptor.Kind] = descriptor
	}
	return result
}()

func All() []Descriptor {
	return append([]Descriptor(nil), descriptors...)
}

func Lookup(kind string) (Descriptor, bool) {
	descriptor, ok := byKind[kind]
	return descriptor, ok
}
