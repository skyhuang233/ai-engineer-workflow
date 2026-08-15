package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ApprovalSnapshot binds every local discovery fact that authorizes an
// Onboarding Plan. Remote HEAD and policy remain separately live preconditions.
type ApprovalSnapshot struct {
	Root                  string         `json:"root"`
	Branch                string         `json:"branch"`
	Head                  string         `json:"head"`
	HasCommits            bool           `json:"has_commits"`
	Origin                string         `json:"origin"`
	Repository            string         `json:"repository"`
	AuthenticatedCloneURL string         `json:"authenticated_clone_url"`
	StatusSHA256          string         `json:"status_sha256"`
	ManagedBoundarySHA256 string         `json:"managed_boundary_sha256"`
	ZeroBaseline          []BaselineFile `json:"zero_baseline,omitempty"`
}

// ApprovalTransitions contains only effect results durably recorded for this
// approved Setup Plan. It must never be inferred from a matching-looking tree.
type ApprovalTransitions struct {
	CreatedRepository    string
	PublishedHistoryHead string
	InitialBaselineHead  string
	MergedHead           string
}

func CaptureApprovalSnapshot(ctx context.Context, discovery Discovery, intendedRepository string, zeroBaseline []BaselineFile) (string, error) {
	status, err := gitBytes(ctx, discovery.Root, "status", "--porcelain=v2", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	managed, err := managedBoundaryDigest(discovery.Root)
	if err != nil {
		return "", err
	}
	cloneURL := ""
	if intendedRepository != "" {
		cloneURL, err = GitHubHTTPSURL(intendedRepository)
		if err != nil {
			return "", err
		}
	}
	snapshot := ApprovalSnapshot{
		Root: discovery.Root, Branch: discovery.Branch, Head: discovery.Head, HasCommits: discovery.HasCommits,
		Origin: discovery.Origin, Repository: discovery.Repository, AuthenticatedCloneURL: cloneURL,
		StatusSHA256: digestSnapshotBytes(status), ManagedBoundarySHA256: managed, ZeroBaseline: zeroBaseline,
	}
	encoded, err := json.Marshal(snapshot)
	return string(encoded), err
}

func VerifyApprovalSnapshot(ctx context.Context, encoded string) error {
	return VerifyApprovalSnapshotTransitions(ctx, encoded, ApprovalTransitions{})
}

func VerifyApprovalSnapshotTransitions(ctx context.Context, encoded string, transitions ApprovalTransitions) error {
	var expected ApprovalSnapshot
	if err := json.Unmarshal([]byte(encoded), &expected); err != nil || expected.Root == "" || expected.Branch == "" || expected.StatusSHA256 == "" || expected.ManagedBoundarySHA256 == "" {
		return errors.New("approved onboarding discovery snapshot is invalid")
	}
	root, err := gitOutput(ctx, expected.Root, "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("approved repository root is no longer a Git repository")
	}
	root, _ = filepath.Abs(root)
	branch, _ := gitOutput(ctx, root, "branch", "--show-current")
	head, headErr := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD")
	hasCommits := headErr == nil
	origin, originErr := gitOutput(ctx, root, "remote", "get-url", "origin")
	if originErr != nil {
		origin = ""
	}
	repository := ""
	cloneURL := ""
	if origin != "" {
		repository, err = parseGitHubOrigin(origin)
		if err != nil {
			return err
		}
		cloneURL, err = GitHubHTTPSURL(repository)
		if err != nil {
			return err
		}
	}
	status, err := gitBytes(ctx, root, "status", "--porcelain=v2", "--untracked-files=all")
	if err != nil {
		return err
	}
	managed, err := managedBoundaryDigest(root)
	if err != nil {
		return err
	}
	baseline := []BaselineFile(nil)
	if !hasCommits {
		baseline, err = BaselineSnapshot(ctx, root)
		if err != nil {
			return err
		}
	}
	actual := ApprovalSnapshot{Root: root, Branch: branch, Head: head, HasCommits: hasCommits, Origin: origin, Repository: repository, AuthenticatedCloneURL: cloneURL, StatusSHA256: digestSnapshotBytes(status), ManagedBoundarySHA256: managed, ZeroBaseline: baseline}
	if actual.Root != expected.Root || actual.Branch != expected.Branch {
		return errors.New("onboarding discovery drifted from the approved snapshot")
	}
	forwardHead := head == transitions.InitialBaselineHead && fullSHA.MatchString(transitions.InitialBaselineHead) || head == transitions.MergedHead && fullSHA.MatchString(transitions.MergedHead)
	if head != expected.Head && !forwardHead || hasCommits != expected.HasCommits && !forwardHead {
		return errors.New("onboarding discovery HEAD drifted from the approved plan transition")
	}
	approvedForwardRepository := strings.TrimPrefix(strings.TrimSuffix(expected.AuthenticatedCloneURL, ".git"), "https://github.com/")
	originForward := expected.Origin == "" && transitions.CreatedRepository == approvedForwardRepository && (transitions.PublishedHistoryHead != "" || transitions.InitialBaselineHead != "") && origin == expected.AuthenticatedCloneURL && repository == approvedForwardRepository && cloneURL == expected.AuthenticatedCloneURL
	exactOrigin := origin == expected.Origin && repository == expected.Repository && (expected.Repository == "" && cloneURL == "" || expected.Repository != "" && cloneURL == expected.AuthenticatedCloneURL)
	if !exactOrigin && !originForward {
		return errors.New("onboarding discovery origin drifted from the approved plan transition")
	}
	wantStatus := expected.StatusSHA256
	baselineTransition := !expected.HasCommits && head == transitions.InitialBaselineHead && fullSHA.MatchString(transitions.InitialBaselineHead)
	mergeAfterBaseline := !expected.HasCommits && head == transitions.MergedHead && fullSHA.MatchString(transitions.MergedHead)
	if mergeAfterBaseline {
		wantStatus = digestSnapshotBytes(nil)
	}
	if !baselineTransition && actual.StatusSHA256 != wantStatus {
		return errors.New("onboarding discovery dirty state drifted from the approved snapshot")
	}
	if actual.ManagedBoundarySHA256 != expected.ManagedBoundarySHA256 && head != transitions.MergedHead {
		return errors.New("onboarding managed boundary drifted without the approved merge evidence")
	}
	if !expected.HasCommits && !hasCommits {
		expectedBaselineJSON, _ := json.Marshal(expected.ZeroBaseline)
		actualBaselineJSON, _ := json.Marshal(actual.ZeroBaseline)
		if string(expectedBaselineJSON) != string(actualBaselineJSON) {
			return errors.New("Initial Repository Baseline content drifted from the approved snapshot")
		}
	}
	if baselineTransition {
		if err := VerifyInitialBaseline(ctx, root, head, expected.ZeroBaseline); err != nil {
			return err
		}
		workingBaseline, baselineErr := BaselineSnapshot(ctx, root)
		if baselineErr != nil {
			return baselineErr
		}
		expectedBaselineJSON, _ := json.Marshal(expected.ZeroBaseline)
		workingBaselineJSON, _ := json.Marshal(workingBaseline)
		if string(expectedBaselineJSON) != string(workingBaselineJSON) {
			return errors.New("Initial Repository Baseline working tree drifted from the approved snapshot")
		}
		if err := verifyBaselineSnapshot(ctx, root, expected.ZeroBaseline); err != nil {
			return err
		}
	}
	return nil
}

func managedBoundaryDigest(root string) (string, error) {
	hash := sha256.New()
	for _, relative := range []string{".workflow/repository.json", "docs/agents/issue-tracker.md", "docs/agents/domain.md", ".github/workflows/workflow-contract.yml"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if errors.Is(err, os.ErrNotExist) {
			data = nil
		} else if err != nil {
			return "", err
		}
		hash.Write([]byte(relative + "\x00"))
		hash.Write(data)
		hash.Write([]byte{0})
	}
	agents, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if errors.Is(err, os.ErrNotExist) {
		agents = nil
	} else if err != nil {
		return "", err
	}
	block, ok := extractManagedBlock(string(agents))
	if !ok {
		block = ""
	}
	hash.Write([]byte("AGENTS.md managed block\x00" + block))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func digestSnapshotBytes(value []byte) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(string(value), "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}
