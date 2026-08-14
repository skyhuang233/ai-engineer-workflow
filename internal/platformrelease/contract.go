package platformrelease

type PlatformSetupContract struct {
	WorkflowHomeDefault string                `json:"workflow_home_default"`
	Credential          CredentialContract    `json:"credential"`
	Docker              DockerDependency      `json:"docker_desktop"`
	Worker              WorkerPin             `json:"worker"`
	SkillBundle         SkillBundleContract   `json:"workflow_skill_bundle"`
	RepositoryContract  RepositoryContractPin `json:"repository_contract"`
}

type CredentialContract struct {
	Kind                  string   `json:"kind"`
	RequiredScopes        []string `json:"required_scopes"`
	OwnerBinding          string   `json:"owner_binding"`
	PlaintextRelativePath string   `json:"plaintext_relative_path"`
}

type DockerDependency struct {
	Version            string `json:"version"`
	InstallerURL       string `json:"installer_url"`
	WindowsAMD64SHA256 string `json:"windows_amd64_sha256"`
}

type WorkerPin struct {
	Image string `json:"image"`
}

type SkillBundleContract struct {
	Version       string   `json:"version"`
	InstallScope  string   `json:"install_scope"`
	ManagedSkills []string `json:"managed_skills"`
}

type RepositoryContractPin struct {
	Version      string            `json:"version"`
	ManifestPath string            `json:"manifest_path"`
	CheckName    string            `json:"check_name"`
	Labels       []RepositoryLabel `json:"labels"`
}

// RepositoryLabel is platform-owned vocabulary. Repository Onboarding only
// reconciles this release-declared contract; it never invents another set.
type RepositoryLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func (r SchemaRange) Supports(schema int) bool {
	return r.MinimumSchema > 0 && r.MaximumSchema >= r.MinimumSchema && schema >= r.MinimumSchema && schema <= r.MaximumSchema
}
