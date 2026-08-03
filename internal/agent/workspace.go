package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

type WorkspaceManager struct {
	RootDir        string
	CodexStateRoot string
}

type workspace struct {
	Path       string
	CodexState string
	Branch     string
	BaseCommit string
}

type RecoveryInspector struct {
	Containers worker.ContainerInspector
	Workspace  WorkspaceManager
}

func (r RecoveryInspector) ContainerRunning(ctx context.Context, runID string) (bool, error) {
	if r.Containers == nil {
		return false, errors.New("container recovery inspector is incomplete")
	}
	return r.Containers.ContainerRunning(ctx, runID)
}

func (r RecoveryInspector) WorkspaceAvailable(_ context.Context, session store.TicketSession) (bool, error) {
	for _, path := range []string{session.WorkspacePath, session.CodexStatePath} {
		if strings.TrimSpace(path) == "" {
			return false, nil
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

func (m WorkspaceManager) ReclaimClosed(ctx context.Context, db *store.Store, retention time.Duration, now time.Time) (int, error) {
	if db == nil || retention <= 0 {
		return 0, errors.New("workspace cleanup configuration is incomplete")
	}
	sessions, err := db.ClosedSessionsForWorkspaceCleanup(ctx, retention, now)
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	var cleanupErr error
	for _, session := range sessions {
		workspacePath, err := managedPath(m.RootDir, session.WorkspacePath)
		if err == nil {
			err = os.RemoveAll(workspacePath)
		}
		if err == nil {
			statePath, stateErr := managedPath(m.CodexStateRoot, session.CodexStatePath)
			if stateErr != nil {
				err = stateErr
			} else {
				err = os.RemoveAll(statePath)
			}
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		if err := db.MarkWorkspaceReclaimed(ctx, session.SessionID, now); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		reclaimed++
	}
	return reclaimed, cleanupErr
}

func managedPath(root, target string) (string, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(target) == "" {
		return "", errors.New("managed workspace path is incomplete")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("workspace path is outside its managed root")
	}
	return target, nil
}

func (m WorkspaceManager) ensure(ctx context.Context, sessionID, sourceRepository, branch string) (workspace, error) {
	if m.RootDir == "" || m.CodexStateRoot == "" || sessionID == "" || sourceRepository == "" || branch == "" {
		return workspace{}, errors.New("workspace configuration is incomplete")
	}
	sourceRepository, err := localSourceRepository(sourceRepository)
	if err != nil {
		return workspace{}, err
	}
	path := filepath.Join(m.RootDir, sessionID)
	state := filepath.Join(m.CodexStateRoot, sessionID)
	if err := os.MkdirAll(m.RootDir, 0o755); err != nil {
		return workspace{}, err
	}
	if err := os.MkdirAll(m.CodexStateRoot, 0o755); err != nil {
		return workspace{}, err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := runGit(ctx, "", "clone", "--local", sourceRepository, path); err != nil {
			return workspace{}, fmt.Errorf("clone ticket workspace: %w", err)
		}
		if err := runGit(ctx, path, "checkout", "-b", branch); err != nil {
			return workspace{}, fmt.Errorf("create ticket branch: %w", err)
		}
	} else if err != nil {
		return workspace{}, err
	} else {
		current, err := gitOutput(ctx, path, "branch", "--show-current")
		if err != nil {
			return workspace{}, err
		}
		if strings.TrimSpace(current) != branch {
			return workspace{}, fmt.Errorf("workspace branch is %q, want %q", strings.TrimSpace(current), branch)
		}
	}
	if err := validateLocalRemotes(ctx, path); err != nil {
		return workspace{}, err
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return workspace{}, err
	}
	base, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return workspace{}, err
	}
	return workspace{Path: path, CodexState: state, Branch: branch, BaseCommit: strings.TrimSpace(base)}, nil
}

func validateLocalRemotes(ctx context.Context, path string) error {
	remotes, err := gitOutput(ctx, path, "remote")
	if err != nil {
		return err
	}
	for _, remote := range strings.Fields(remotes) {
		for _, push := range []bool{false, true} {
			args := []string{"remote", "get-url", "--all"}
			if push {
				args = append(args, "--push")
			}
			args = append(args, remote)
			urls, err := gitOutput(ctx, path, args...)
			if err != nil {
				return err
			}
			for _, remoteURL := range strings.Split(strings.TrimSpace(urls), "\n") {
				remoteURL = strings.TrimSpace(remoteURL)
				if remoteURL == "" {
					continue
				}
				if !filepath.IsAbs(remoteURL) {
					return fmt.Errorf("workspace remote %q must use an absolute local path", remote)
				}
			}
		}
	}
	return nil
}

func localSourceRepository(source string) (string, error) {
	if !filepath.IsAbs(source) {
		return "", errors.New("workspace source repository must be an absolute local path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(source))
	if err != nil {
		return "", fmt.Errorf("resolve workspace source repository: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect workspace source repository: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("workspace source repository must be a local directory")
	}
	return resolved, nil
}

func (m WorkspaceManager) schemaPath(state string) string {
	return filepath.Join(state, "output-schema.json")
}

func (m WorkspaceManager) status(ctx context.Context, ws workspace) (commit, branch string, clean bool, err error) {
	commit, err = gitOutput(ctx, ws.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", "", false, err
	}
	branch, err = gitOutput(ctx, ws.Path, "branch", "--show-current")
	if err != nil {
		return "", "", false, err
	}
	status, err := gitOutput(ctx, ws.Path, "status", "--porcelain")
	if err != nil {
		return "", "", false, err
	}
	return strings.TrimSpace(commit), strings.TrimSpace(branch), strings.TrimSpace(status) == "", nil
}

func (m WorkspaceManager) diagnostic(ctx context.Context, ws workspace, runID, baseCommit, output, runErr string) (string, error) {
	dir := filepath.Join(filepath.Dir(ws.CodexState), "diagnostics", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "report.txt")
	status, statusErr := gitOutput(ctx, ws.Path, "status", "--short")
	if statusErr != nil {
		status = "git status failed: " + statusErr.Error()
	}
	head, headErr := gitOutput(ctx, ws.Path, "rev-parse", "HEAD")
	if headErr != nil {
		head = "git rev-parse HEAD failed: " + headErr.Error()
	}
	diff, diffErr := gitOutput(ctx, ws.Path, "diff", "--binary", baseCommit)
	if diffErr != nil {
		diff = "git diff failed: " + diffErr.Error()
	}
	if err := os.WriteFile(filepath.Join(dir, "revision.patch"), []byte(diff), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "head.txt"), []byte(strings.TrimSpace(head)+"\nbase: "+baseCommit+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := m.copyResidue(ctx, ws, dir, false); err != nil {
		return "", err
	}
	if err := m.copyResidue(ctx, ws, dir, true); err != nil {
		return "", err
	}
	body := "error: " + runErr + "\nhead: " + strings.TrimSpace(head) + "\nbase: " + baseCommit + "\nstatus:\n" + status + "\noutput:\n" + output + "\nevidence: revision.patch, head.txt, residue/, and ignored/"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (m WorkspaceManager) copyResidue(ctx context.Context, ws workspace, dir string, ignored bool) error {
	args := []string{"ls-files", "--others", "--exclude-standard", "-z"}
	destinationRoot := "residue"
	if ignored {
		args = []string{"ls-files", "--others", "--ignored", "--exclude-standard", "-z"}
		destinationRoot = "ignored"
	}
	files, err := gitOutput(ctx, ws.Path, args...)
	if err != nil {
		return err
	}
	for _, name := range strings.Split(files, "\x00") {
		if name == "" {
			continue
		}
		source := filepath.Join(ws.Path, filepath.FromSlash(name))
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if info.IsDir() {
			continue
		}
		destination := filepath.Join(dir, destinationRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (m WorkspaceManager) restore(ctx context.Context, ws workspace, commit string) error {
	if commit == "" {
		return errors.New("restore commit is empty")
	}
	if err := runGit(ctx, ws.Path, "reset", "--hard"); err != nil {
		return err
	}
	if err := runGit(ctx, ws.Path, "switch", "-C", ws.Branch, commit); err != nil {
		return err
	}
	return runGit(ctx, ws.Path, "clean", "-fdx")
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w (%s)", args, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return string(output), nil
}
