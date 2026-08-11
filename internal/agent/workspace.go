package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/codexauth"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/worker"
)

type WorkspaceManager struct {
	RootDir               string
	CodexStateRoot        string
	CodexAuthFile         string
	RefreshDeliverySource func(context.Context, string) (string, error)
}

type workspace struct {
	Path                 string
	CodexState           string
	DeliverySource       string
	DeliverySourceDigest string
	RevisionRoundID      string
	SourceRepository     string
	Branch               string
	BaseCommit           string
}

type deliverySourceInfrastructureFailure struct {
	err error
}

func (e *deliverySourceInfrastructureFailure) Error() string               { return e.err.Error() }
func (e *deliverySourceInfrastructureFailure) Unwrap() error               { return e.err }
func (e *deliverySourceInfrastructureFailure) InfrastructureFailure() bool { return true }

func deliverySourceInfrastructureError(err error) error {
	if err == nil {
		return nil
	}
	var infrastructureFailure *deliverySourceInfrastructureFailure
	if errors.As(err, &infrastructureFailure) {
		return err
	}
	return &deliverySourceInfrastructureFailure{err: err}
}

type deliverySourceIntegrityFailure struct {
	err error
}

func (e *deliverySourceIntegrityFailure) Error() string { return e.err.Error() }
func (e *deliverySourceIntegrityFailure) Unwrap() error { return e.err }

func deliverySourceIntegrityError(err error) error {
	if err == nil {
		return nil
	}
	return &deliverySourceIntegrityFailure{err: err}
}

const admittedSourceRepositoryConfig = "workflow.sourceRepository"

type RecoveryInspector struct {
	Containers worker.ContainerInspector
	Workspace  WorkspaceManager
}

func (m WorkspaceManager) AdmitCodexAuthentication(ctx context.Context, db *store.Store, versionID string, ticketID int64) error {
	if db == nil || versionID == "" || ticketID == 0 || !filepath.IsAbs(m.CodexStateRoot) {
		return errors.New("Codex authentication admission is incomplete")
	}
	session, err := db.TicketSession(ctx, versionID, ticketID)
	if err == nil {
		_, err = m.ProvisionCodexSession(ctx, store.SessionProvisioning{SessionID: session.SessionID, Existing: true, WorkspacePath: session.WorkspacePath, CodexStatePath: session.CodexStatePath})
		return err
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if _, _, err := m.sessionPaths("admission"); err != nil {
		return err
	}
	return codexauth.ValidateChatGPT(m.CodexAuthFile)
}

func (m WorkspaceManager) ProvisionCodexAuthentication(ctx context.Context, sessionID string, existing bool) error {
	_, err := m.ProvisionCodexSession(ctx, store.SessionProvisioning{SessionID: sessionID, Existing: existing})
	return err
}

func (m WorkspaceManager) ProvisionCodexSession(_ context.Context, provisioning store.SessionProvisioning) (store.SessionProvisioningResult, error) {
	workspacePath, state, err := m.sessionPaths(provisioning.SessionID)
	if err != nil {
		return store.SessionProvisioningResult{}, err
	}
	for _, persisted := range []struct {
		name     string
		value    string
		expected string
	}{{name: "Ticket Workspace", value: provisioning.WorkspacePath, expected: workspacePath}, {name: "Codex state", value: provisioning.CodexStatePath, expected: state}} {
		if persisted.value == "" {
			continue
		}
		canonical, err := canonicalPath(persisted.value)
		if err != nil {
			return store.SessionProvisioningResult{}, err
		}
		if !strings.EqualFold(canonical, persisted.expected) {
			return store.SessionProvisioningResult{}, fmt.Errorf("persisted %s path does not match configured Session path", persisted.name)
		}
	}
	authPath := filepath.Join(state, codexauth.FileName)
	if provisioning.Existing {
		if err := codexauth.ValidateChatGPT(authPath); err != nil {
			failure := &store.SessionAuthenticationFailure{}
			if provisioning.CurrentRunID != "" {
				failure.DiagnosticsPath, _ = m.writeMinimalAuthenticationDiagnostic(provisioning.CurrentRunID)
			}
			return store.SessionProvisioningResult{}, failure
		}
		return store.SessionProvisioningResult{}, nil
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return store.SessionProvisioningResult{}, fmt.Errorf("create Ticket Session Codex state: %w", err)
	}
	if err := codexauth.SeedNew(m.CodexAuthFile, state); err != nil {
		return store.SessionProvisioningResult{}, err
	}
	return store.SessionProvisioningResult{Rollback: func() error {
		if err := os.RemoveAll(state); err != nil {
			return fmt.Errorf("remove uncommitted Ticket Session Codex state: %w", err)
		}
		if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("uncommitted Ticket Session Codex state still exists")
			}
			return fmt.Errorf("verify uncommitted Ticket Session Codex state removal: %w", err)
		}
		return nil
	}}, nil
}

func (m WorkspaceManager) writeMinimalAuthenticationDiagnostic(runID string) (string, error) {
	dir := filepath.Join(m.CodexStateRoot, "diagnostics", runID)
	if filepath.Base(runID) != runID {
		return "", errors.New("diagnostic Run ID is invalid")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "report.txt")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	body := "error: " + store.ErrSessionAuthenticationUnavailable.Error() + "\nevidence: detailed evidence omitted\n"
	temporary, err := os.CreateTemp(dir, ".minimal-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.WriteString(body); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	removeTemporary = false
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("minimal diagnostic is not a regular file")
	}
	return path, nil
}

func (r RecoveryInspector) ContainerRunning(ctx context.Context, runID string) (bool, error) {
	if r.Containers == nil {
		return false, errors.New("container recovery inspector is incomplete")
	}
	return r.Containers.ContainerRunning(ctx, runID)
}

func (r RecoveryInspector) IsolateContainer(ctx context.Context, runID string) error {
	if r.Containers == nil {
		return errors.New("container recovery inspector is incomplete")
	}
	isolator, ok := r.Containers.(worker.ContainerIsolator)
	if !ok {
		return errors.New("container recovery inspector cannot isolate expired Worker Runs")
	}
	return isolator.IsolateContainer(ctx, runID)
}

func (r RecoveryInspector) WorkspaceAvailable(_ context.Context, session store.TicketSession) (bool, error) {
	if strings.TrimSpace(session.WorkspacePath) == "" || strings.TrimSpace(session.CodexStatePath) == "" {
		return false, nil
	}
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
		var err error
		if strings.TrimSpace(session.WorkspacePath) != "" {
			workspacePath, workspaceErr := managedPath(m.RootDir, session.WorkspacePath)
			if workspaceErr != nil {
				err = workspaceErr
			} else {
				err = os.RemoveAll(workspacePath)
			}
		}
		if err == nil && strings.TrimSpace(session.CodexStatePath) != "" {
			statePath, stateErr := managedPath(m.CodexStateRoot, session.CodexStatePath)
			if stateErr != nil {
				err = stateErr
			} else {
				err = os.RemoveAll(statePath)
			}
		}
		if err == nil {
			deliverySource, sourceErr := m.deliverySourceSessionPath(session.SessionID)
			if sourceErr != nil {
				err = sourceErr
			} else {
				err = os.RemoveAll(deliverySource)
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

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := filepath.Clean(absolute)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return filepath.Clean(absolute), nil
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func pathsOverlap(first, second string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
	}
	return contains(first, second) || contains(second, first)
}

func (m WorkspaceManager) sessionPaths(sessionID string) (string, string, error) {
	if sessionID == "" || m.RootDir == "" || m.CodexStateRoot == "" {
		return "", "", errors.New("workspace configuration is incomplete")
	}
	workspacePath, err := canonicalPath(filepath.Join(m.RootDir, sessionID))
	if err != nil {
		return "", "", err
	}
	statePath, err := canonicalPath(filepath.Join(m.CodexStateRoot, sessionID))
	if err != nil {
		return "", "", err
	}
	if pathsOverlap(workspacePath, statePath) {
		return "", "", errors.New("Ticket Workspace and Codex state paths overlap")
	}
	return workspacePath, statePath, nil
}

func (m WorkspaceManager) ensure(ctx context.Context, sessionID, revisionRoundID, sourceRepository, branch string) (workspace, error) {
	if m.RootDir == "" || m.CodexStateRoot == "" || sessionID == "" || revisionRoundID == "" || sourceRepository == "" || branch == "" {
		return workspace{}, errors.New("workspace configuration is incomplete")
	}
	sourceRepository, err := localSourceRepository(sourceRepository)
	if err != nil {
		return workspace{}, err
	}
	path, state, err := m.sessionPaths(sessionID)
	if err != nil {
		return workspace{}, err
	}
	if err := os.MkdirAll(m.RootDir, 0o755); err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	if err := os.MkdirAll(m.CodexStateRoot, 0o755); err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	deliverySource, err := m.ensureDeliverySource(ctx, sessionID, revisionRoundID, sourceRepository)
	if err != nil {
		return workspace{}, err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := runGit(ctx, "", "-c", "core.longpaths=true", "clone", "--config", "core.autocrlf=false", "--config", "core.eol=lf", "--config", "core.longpaths=true", "--local", "--no-hardlinks", deliverySource, path); err != nil {
			return workspace{}, deliverySourceInfrastructureError(fmt.Errorf("clone ticket workspace: %w", err))
		}
		if err := runGit(ctx, path, "checkout", "-b", branch); err != nil {
			return workspace{}, deliverySourceInfrastructureError(fmt.Errorf("create ticket branch: %w", err))
		}
	} else if err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	} else {
		current, err := gitOutput(ctx, path, "branch", "--show-current")
		if err != nil {
			return workspace{}, deliverySourceInfrastructureError(err)
		}
		if strings.TrimSpace(current) != branch {
			return workspace{}, fmt.Errorf("workspace branch is %q, want %q", strings.TrimSpace(current), branch)
		}
	}
	if err := recordAdmittedSourceRepository(ctx, path, sourceRepository); err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	if err := runGit(ctx, path, "fetch", "--force", "--prune", "--no-tags", deliverySource, "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return workspace{}, deliverySourceInfrastructureError(fmt.Errorf("refresh ticket workspace source: %w", err))
	}
	if err := replaceWorkspaceOriginURLs(ctx, path, []string{sourceRepository}); err != nil {
		return workspace{}, deliverySourceInfrastructureError(fmt.Errorf("restore ticket workspace origin: %w", err))
	}
	if err := configureTicketWorkspaceLineEndings(ctx, path); err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	if err := configureTicketWorkspaceGitIdentity(ctx, path); err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	if err := validateLocalRemotes(ctx, path); err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	base, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return workspace{}, deliverySourceInfrastructureError(err)
	}
	deliverySourceDigest, err := digestDeliverySource(ctx, deliverySource)
	if err != nil {
		return workspace{}, err
	}
	return workspace{Path: path, CodexState: state, DeliverySource: deliverySource, DeliverySourceDigest: deliverySourceDigest, RevisionRoundID: revisionRoundID, SourceRepository: sourceRepository, Branch: branch, BaseCommit: strings.TrimSpace(base)}, nil
}

func (m WorkspaceManager) deliverySourceSessionPath(sessionID string) (string, error) {
	if strings.TrimSpace(m.RootDir) == "" {
		return "", deliverySourceIntegrityError(errors.New("workspace configuration is incomplete"))
	}
	if sessionID == "" || filepath.Base(sessionID) != sessionID {
		return "", deliverySourceIntegrityError(errors.New("Ticket Session ID is invalid"))
	}
	workspaceRoot, err := canonicalPath(m.RootDir)
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("resolve workspace root: %w", err))
	}
	root, err := canonicalPath(filepath.Join(workspaceRoot, ".delivery-sources"))
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("resolve Delivery Source root: %w", err))
	}
	if _, err := managedPath(workspaceRoot, root); err != nil {
		return "", deliverySourceIntegrityError(err)
	}
	path, err := canonicalPath(filepath.Join(root, sessionID))
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("resolve Ticket Session Delivery Source path: %w", err))
	}
	if _, err := managedPath(root, path); err != nil {
		return "", deliverySourceIntegrityError(err)
	}
	return path, nil
}

func (m WorkspaceManager) deliverySourcePath(sessionID, revisionRoundID string) (string, error) {
	if revisionRoundID == "" || filepath.Base(revisionRoundID) != revisionRoundID {
		return "", deliverySourceIntegrityError(errors.New("Revision Round ID is invalid"))
	}
	root, err := m.deliverySourceSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	path, err := canonicalPath(filepath.Join(root, revisionRoundID+".git"))
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("resolve Revision Round Delivery Source path: %w", err))
	}
	if _, err := managedPath(root, path); err != nil {
		return "", deliverySourceIntegrityError(err)
	}
	return path, nil
}

func (m WorkspaceManager) ensureDeliverySource(ctx context.Context, sessionID, revisionRoundID, sourceRepository string) (string, error) {
	path, err := m.deliverySourcePath(sessionID, revisionRoundID)
	if err != nil {
		return "", err
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(sourceRepository)))
	if info, statErr := os.Stat(path); statErr == nil {
		if !info.IsDir() {
			return "", deliverySourceInfrastructureError(errors.New("persisted Delivery Source is not a directory"))
		}
		storedIdentity, err := gitOutput(ctx, path, "config", "--local", "--get", "workflow.sourceIdentity")
		if err != nil || strings.TrimSpace(storedIdentity) != identity {
			return "", deliverySourceInfrastructureError(errors.New("persisted Delivery Source identity does not match the admitted source repository"))
		}
		if err := validateDeliverySource(ctx, path); err != nil {
			return "", err
		}
		return path, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", deliverySourceInfrastructureError(statErr)
	}
	root := filepath.Dir(path)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", deliverySourceInfrastructureError(err)
	}
	temporaryPath, err := os.MkdirTemp(root, ".delivery-")
	if err != nil {
		return "", deliverySourceInfrastructureError(err)
	}
	defer os.RemoveAll(temporaryPath)
	if err := runGit(ctx, "", "-c", "core.longpaths=true", "init", "--bare", "--template=", temporaryPath); err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("initialize Delivery Source: %w", err))
	}
	if err := runGit(ctx, temporaryPath, "config", "--local", "core.longpaths", "true"); err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("configure Delivery Source long paths: %w", err))
	}
	if err := runGit(ctx, temporaryPath, "fetch", "--force", "--prune", "--no-tags", sourceRepository, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("copy admitted Delivery Source: %w", err))
	}
	var head string
	if m.RefreshDeliverySource != nil {
		head, err = m.RefreshDeliverySource(ctx, temporaryPath)
		if err != nil {
			return "", deliverySourceInfrastructureError(fmt.Errorf("refresh Delivery Source from admitted remote: %w", err))
		}
	} else {
		head, err = gitOutput(ctx, sourceRepository, "symbolic-ref", "--quiet", "HEAD")
		if err != nil {
			return "", deliverySourceInfrastructureError(fmt.Errorf("resolve Delivery Source HEAD: %w", err))
		}
	}
	head = strings.TrimSpace(head)
	if _, err := validateDeliverySourceDefaultBranchRef(ctx, temporaryPath, head); err != nil {
		return "", deliverySourceInfrastructureError(err)
	}
	if err := runGit(ctx, temporaryPath, "symbolic-ref", "HEAD", head); err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("record Delivery Source HEAD: %w", err))
	}
	if err := runGit(ctx, temporaryPath, "config", "--local", "workflow.sourceIdentity", identity); err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("record Delivery Source identity: %w", err))
	}
	if err := validateDeliverySource(ctx, temporaryPath); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("persist Delivery Source: %w", err))
	}
	return path, nil
}

func (m WorkspaceManager) reclaimSupersededDeliverySources(ctx context.Context, sessionID, revisionRoundID string) error {
	current, err := m.deliverySourcePath(sessionID, revisionRoundID)
	if err != nil {
		return err
	}
	if err := validateDeliverySource(ctx, current); err != nil {
		return err
	}
	root := filepath.Dir(current)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		candidate := filepath.Join(root, entry.Name())
		if strings.EqualFold(candidate, current) {
			continue
		}
		candidate, err = managedPath(root, candidate)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(candidate); err != nil {
			return err
		}
	}
	return nil
}

func (m WorkspaceManager) sealDeliverySource(ctx context.Context, sessionID, revisionRoundID, launchID, sourcePath, expectedDigest string) (string, func() error, error) {
	current, err := m.deliverySourcePath(sessionID, revisionRoundID)
	if err != nil {
		return "", nil, err
	}
	if filepath.Clean(sourcePath) != current {
		return "", nil, deliverySourceIntegrityError(errors.New("Delivery Source path does not match the accepted Revision Round"))
	}
	if launchID == "" || filepath.Base(launchID) != launchID {
		return "", nil, deliverySourceIntegrityError(errors.New("Delivery Controller launch ID is invalid"))
	}
	root := filepath.Dir(current)
	launchPath, err := canonicalPath(filepath.Join(root, ".launch-"+launchID+".git"))
	if err != nil {
		return "", nil, deliverySourceInfrastructureError(fmt.Errorf("resolve sealed Delivery Source path: %w", err))
	}
	if _, err := managedPath(root, launchPath); err != nil {
		return "", nil, deliverySourceIntegrityError(err)
	}
	temporaryPath := launchPath + ".tmp"
	for _, path := range []string{launchPath, temporaryPath} {
		if err := makeDeliverySourceWritable(path); err != nil {
			return "", nil, deliverySourceInfrastructureError(fmt.Errorf("unlock stale sealed Delivery Source: %w", err))
		}
		if err := os.RemoveAll(path); err != nil {
			return "", nil, deliverySourceInfrastructureError(fmt.Errorf("remove stale sealed Delivery Source: %w", err))
		}
	}
	cleanup := func() error {
		return errors.Join(makeDeliverySourceWritable(launchPath), os.RemoveAll(launchPath), makeDeliverySourceWritable(temporaryPath), os.RemoveAll(temporaryPath))
	}
	identity, err := gitOutput(ctx, sourcePath, "config", "--local", "--get", "workflow.sourceIdentity")
	if err != nil {
		return "", cleanup, deliverySourceInfrastructureError(fmt.Errorf("read Delivery Source identity: %w", err))
	}
	if err := runGit(ctx, "", "clone", "--bare", "--no-hardlinks", sourcePath, temporaryPath); err != nil {
		return "", cleanup, deliverySourceInfrastructureError(fmt.Errorf("seal Delivery Source: %w", err))
	}
	if err := runGit(ctx, temporaryPath, "remote", "remove", "origin"); err != nil {
		return "", cleanup, deliverySourceInfrastructureError(fmt.Errorf("detach sealed Delivery Source: %w", err))
	}
	if err := runGit(ctx, temporaryPath, "config", "--local", "workflow.sourceIdentity", strings.TrimSpace(identity)); err != nil {
		return "", cleanup, deliverySourceInfrastructureError(fmt.Errorf("record sealed Delivery Source identity: %w", err))
	}
	if err := verifyDeliverySourceDigest(ctx, temporaryPath, expectedDigest); err != nil {
		return "", cleanup, err
	}
	if err := os.Rename(temporaryPath, launchPath); err != nil {
		return "", cleanup, deliverySourceInfrastructureError(fmt.Errorf("persist sealed Delivery Source: %w", err))
	}
	if err := makeDeliverySourceReadOnly(launchPath); err != nil {
		return "", cleanup, deliverySourceInfrastructureError(fmt.Errorf("seal Delivery Source permissions: %w", err))
	}
	return launchPath, cleanup, nil
}

func makeDeliverySourceReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	})
}

func makeDeliverySourceWritable(root string) error {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	})
}

func validateDeliverySource(ctx context.Context, path string) error {
	bare, err := gitOutput(ctx, path, "rev-parse", "--is-bare-repository")
	if err != nil {
		return deliverySourceInfrastructureError(fmt.Errorf("inspect persisted Delivery Source: %w", err))
	}
	if strings.TrimSpace(bare) != "true" {
		return deliverySourceIntegrityError(errors.New("persisted Delivery Source is not a bare Git repository"))
	}
	remotes, err := gitOutput(ctx, path, "remote")
	if err != nil {
		return deliverySourceInfrastructureError(fmt.Errorf("inspect persisted Delivery Source remotes: %w", err))
	}
	if strings.TrimSpace(remotes) != "" {
		return deliverySourceIntegrityError(errors.New("persisted Delivery Source contains a remote configuration"))
	}
	return nil
}

func validateDeliverySourceDefaultBranchRef(ctx context.Context, sourcePath, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", deliverySourceIntegrityError(errors.New("Delivery Source did not identify a default branch"))
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if _, err := gitOutput(ctx, sourcePath, "check-ref-format", "--branch", branch); err != nil {
		return "", deliverySourceIntegrityError(fmt.Errorf("invalid Delivery Source default branch %q: %w", branch, err))
	}
	if _, err := gitOutput(ctx, sourcePath, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
		return "", deliverySourceIntegrityError(fmt.Errorf("resolve Delivery Source default branch %q: %w", branch, err))
	}
	return branch, nil
}

func deliverySourceDefaultBranch(ctx context.Context, sourcePath string) (string, string, error) {
	if strings.TrimSpace(sourcePath) == "" {
		return "", "", deliverySourceIntegrityError(errors.New("Delivery Source path is required"))
	}
	output, err := gitOutput(ctx, sourcePath, "symbolic-ref", "HEAD")
	if err != nil {
		return "", "", deliverySourceInfrastructureError(fmt.Errorf("read Delivery Source HEAD: %w", err))
	}
	ref := strings.TrimSpace(output)
	branch, err := validateDeliverySourceDefaultBranchRef(ctx, sourcePath, ref)
	if err != nil {
		return "", "", err
	}
	return ref, branch, nil
}

func digestDeliverySource(ctx context.Context, sourcePath string) (string, error) {
	head, _, err := deliverySourceDefaultBranch(ctx, sourcePath)
	if err != nil {
		return "", err
	}
	identity, err := gitOutput(ctx, sourcePath, "config", "--local", "--get", "workflow.sourceIdentity")
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("read Delivery Source identity: %w", err))
	}
	if err := runGit(ctx, sourcePath, "fsck", "--connectivity-only", "--no-dangling"); err != nil {
		if ctx.Err() != nil {
			return "", deliverySourceInfrastructureError(fmt.Errorf("validate Delivery Source connectivity: %w", ctx.Err()))
		}
		return "", deliverySourceIntegrityError(fmt.Errorf("Delivery Source contains missing or corrupt reachable objects: %w", err))
	}
	refs, err := gitOutput(ctx, sourcePath, "for-each-ref", "--sort=refname", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("read Delivery Source refs: %w", err))
	}
	digest := sha256.Sum256([]byte(head + "\n" + strings.TrimSpace(identity) + "\n" + strings.TrimSpace(refs)))
	return fmt.Sprintf("%x", digest), nil
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
				if !filepath.IsAbs(remoteURL) && !pathpkg.IsAbs(remoteURL) {
					return fmt.Errorf("workspace remote %q must use an absolute local path", remote)
				}
			}
		}
	}
	return nil
}

func replaceWorkspaceOriginURLs(ctx context.Context, workspacePath string, urls []string) error {
	if len(urls) == 0 || strings.TrimSpace(urls[0]) == "" {
		return errors.New("Ticket Workspace origin is required")
	}
	if err := runGit(ctx, workspacePath, "config", "--local", "--replace-all", "remote.origin.url", urls[0]); err != nil {
		return err
	}
	for _, remoteURL := range urls[1:] {
		if strings.TrimSpace(remoteURL) == "" {
			return errors.New("Ticket Workspace origin is required")
		}
		if err := runGit(ctx, workspacePath, "config", "--local", "--add", "remote.origin.url", remoteURL); err != nil {
			return err
		}
	}
	selected, err := gitOutput(ctx, workspacePath, "config", "--local", "--get-all", "remote.origin.url")
	if err != nil {
		return err
	}
	if strings.TrimSpace(selected) != strings.Join(urls, "\n") {
		return errors.New("Ticket Workspace origin URLs were not configured exactly")
	}
	return nil
}

func recordAdmittedSourceRepository(ctx context.Context, workspacePath, sourceRepository string) error {
	if err := runGit(ctx, workspacePath, "config", "--local", admittedSourceRepositoryConfig, sourceRepository); err != nil {
		return fmt.Errorf("record admitted source repository: %w", err)
	}
	recorded, err := gitOutput(ctx, workspacePath, "config", "--local", "--get", admittedSourceRepositoryConfig)
	if err != nil {
		return fmt.Errorf("verify admitted source repository: %w", err)
	}
	if strings.TrimSpace(recorded) != sourceRepository {
		return errors.New("admitted source repository was not recorded exactly")
	}
	return nil
}

func admittedSourceRepository(ctx context.Context, workspacePath, configured string) (string, error) {
	recorded, err := gitOutput(ctx, workspacePath, "config", "--local", "--get", admittedSourceRepositoryConfig)
	if err == nil && strings.TrimSpace(recorded) != "" {
		return validateAdmittedSourceRepository(strings.TrimSpace(recorded))
	}
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		return "", deliverySourceInfrastructureError(fmt.Errorf("read admitted source repository: %w", err))
	}
	if strings.TrimSpace(configured) != "" {
		return validateAdmittedSourceRepository(configured)
	}
	return "", deliverySourceIntegrityError(errors.New("Ticket Workspace admitted source repository is unavailable"))
}

func validateAdmittedSourceRepository(sourceRepository string) (string, error) {
	sourceRepository = filepath.Clean(strings.TrimSpace(sourceRepository))
	if !filepath.IsAbs(sourceRepository) {
		return "", deliverySourceIntegrityError(errors.New("Ticket Workspace admitted source repository must be an absolute local path"))
	}
	return sourceRepository, nil
}

func localSourceRepository(source string) (string, error) {
	if !filepath.IsAbs(source) {
		return "", deliverySourceIntegrityError(errors.New("workspace source repository must be an absolute local path"))
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(source))
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("resolve workspace source repository: %w", err))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", deliverySourceInfrastructureError(fmt.Errorf("inspect workspace source repository: %w", err))
	}
	if !info.IsDir() {
		return "", deliverySourceIntegrityError(errors.New("workspace source repository must be a local directory"))
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

func (m WorkspaceManager) authenticationRedactor(ws workspace) (codexauth.Redactor, error) {
	if m.CodexAuthFile == "" {
		return codexauth.Redactor{}, nil
	}
	return codexauth.NewRedactor(filepath.Join(ws.CodexState, codexauth.FileName))
}

func (m WorkspaceManager) diagnostic(ctx context.Context, ws workspace, runID, baseCommit, output, runErr string, redactor *codexauth.Redactor) (string, error) {
	dir := filepath.Join(filepath.Dir(ws.CodexState), "diagnostics", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "report.txt")
	if redactor == nil {
		body := "error: worker failed; detailed evidence omitted because Codex authentication could not be safely redacted\n" +
			"head: unavailable\n" +
			"base: " + baseCommit + "\n" +
			"evidence: detailed evidence omitted\n"
		if writeErr := os.WriteFile(path, []byte(body), 0o600); writeErr != nil {
			return "", writeErr
		}
		return path, nil
	}
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
	if err := os.WriteFile(filepath.Join(dir, "revision.patch"), redactor.Bytes([]byte(diff)), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "head.txt"), redactor.Bytes([]byte(strings.TrimSpace(head)+"\nbase: "+baseCommit+"\n")), 0o600); err != nil {
		return "", err
	}
	if err := m.copyResidue(ctx, ws, dir, false, *redactor); err != nil {
		return "", err
	}
	if err := m.copyResidue(ctx, ws, dir, true, *redactor); err != nil {
		return "", err
	}
	body := "error: " + runErr + "\nhead: " + strings.TrimSpace(head) + "\nbase: " + baseCommit + "\nstatus:\n" + status + "\noutput:\n" + output + "\nevidence: revision.patch, head.txt, residue/, and ignored/"
	if err := os.WriteFile(path, redactor.Bytes([]byte(body)), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (m WorkspaceManager) copyResidue(ctx context.Context, ws workspace, dir string, ignored bool, redactor codexauth.Redactor) error {
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
		if err := os.WriteFile(destination, redactor.Bytes(data), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (m WorkspaceManager) restore(ctx context.Context, ws workspace, commit string) error {
	if commit == "" {
		return errors.New("restore commit is empty")
	}
	if err := configureTicketWorkspaceLineEndings(ctx, ws.Path); err != nil {
		return err
	}
	if err := runGit(ctx, ws.Path, "reset", "--hard"); err != nil {
		return err
	}
	if err := runGit(ctx, ws.Path, "switch", "-C", ws.Branch, commit); err != nil {
		return err
	}
	return runGit(ctx, ws.Path, "clean", "-fdx")
}

func configureTicketWorkspaceLineEndings(ctx context.Context, path string) error {
	if err := runGit(ctx, path, "config", "--local", "core.longpaths", "true"); err != nil {
		return fmt.Errorf("configure Ticket Workspace long paths: %w", err)
	}
	if err := runGit(ctx, path, "config", "--local", "core.autocrlf", "false"); err != nil {
		return fmt.Errorf("configure Ticket Workspace autocrlf: %w", err)
	}
	if err := runGit(ctx, path, "config", "--local", "core.eol", "lf"); err != nil {
		return fmt.Errorf("configure Ticket Workspace line ending: %w", err)
	}
	return nil
}

func configureTicketWorkspaceGitIdentity(ctx context.Context, path string) error {
	for key, value := range map[string]string{
		"user.name":  "workflow-ticket-agent",
		"user.email": "workflow-ticket-agent@users.noreply.github.com",
	} {
		if err := runGit(ctx, path, "config", "--local", key, value); err != nil {
			return fmt.Errorf("configure Ticket Workspace %s: %w", key, err)
		}
	}
	return nil
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

func trustedGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.HasPrefix(strings.ToUpper(name), "GIT_CONFIG_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=0", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return string(output), nil
}
