// Package setupcontract defines the immutable, digest-bound authority used by
// Repository Onboarding. It deliberately contains no platform mutation logic.
package setupcontract

import "time"

const SchemaVersion = 1

type PlanKind string

const (
	RepositoryOnboarding PlanKind = "repository_onboarding"
)

type Plan struct {
	SchemaVersion   int              `json:"schema_version"`
	PlanID          string           `json:"plan_id"`
	Kind            PlanKind         `json:"kind"`
	Target          Target           `json:"target"`
	Preconditions   []Precondition   `json:"preconditions"`
	Effects         []Effect         `json:"effects"`
	ExpectedResults []ExpectedResult `json:"expected_results"`
}

type Target struct {
	WorkflowHome     string `json:"workflow_home"`
	RepositoryPath   string `json:"repository_path"`
	GitHubRepository string `json:"github_repository"`
}

type Precondition struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	Expected string `json:"expected"`
}

type Effect struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Subject    string            `json:"subject"`
	Action     string            `json:"action"`
	Parameters map[string]string `json:"parameters"`
}

type ExpectedResult struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	Expected string `json:"expected"`
}

type ExecutionStatus string

const (
	ExecutionSucceeded  ExecutionStatus = "succeeded"
	ExecutionIncomplete ExecutionStatus = "incomplete"
	ExecutionDrifted    ExecutionStatus = "drifted"
	ExecutionBlocked    ExecutionStatus = "blocked"
)

type EffectStatus string

const (
	EffectSatisfied   EffectStatus = "satisfied"
	EffectRequired    EffectStatus = "required"
	EffectConflicting EffectStatus = "conflicting"
	EffectFailed      EffectStatus = "failed"
)

// ExecutionResult is one immutable attempt record. Persistence is expected to
// append these records; a later attempt never rewrites an earlier result.
type ExecutionResult struct {
	SchemaVersion int             `json:"schema_version"`
	PlanID        string          `json:"plan_id"`
	PlanDigest    string          `json:"plan_digest"`
	AttemptID     string          `json:"attempt_id"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at"`
	Status        ExecutionStatus `json:"status"`
	Effects       []EffectResult  `json:"effects"`
	Blocker       string          `json:"blocker"`
}

type EffectResult struct {
	EffectID string       `json:"effect_id"`
	Status   EffectStatus `json:"status"`
	Evidence string       `json:"evidence"`
}
