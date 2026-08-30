package launcher

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/githubcredential"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowbundle"
	"github.com/skyhuang233/workflow/internal/workflowrelease"
)

func TestVerifiedReleaseManifestSeedsActiveWorkerRelease(t *testing.T) {
	for _, qualification := range []bool{false, true} {
		name := "published"
		if qualification {
			name = "qualification"
		}
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			bundle := t.TempDir()
			writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
			manifestDirectory := t.TempDir()
			sourceCommit := strings.Repeat("c", 40)
			bundleDigest := strings.Repeat("b", 64)
			image := "ghcr.io/skyhuang233/workflow-worker@sha256:" + strings.Repeat("a", 64)
			manifest := workflowrelease.Manifest{
				SchemaVersion: 1, Version: "0.0.1", CandidateSourceCommit: sourceCommit, QualificationRunID: 1, QualificationRunAttempt: 1,
				Bundle: workflowrelease.Bundle{Name: workflowrelease.BundleAssetName, SHA256: bundleDigest},
				Worker: workflowrelease.Worker{Image: image, Tools: workflowrelease.Tools{
					Codex:      workflowrelease.CodexTool{Version: "0.148.0"},
					GitHubCLI:  workflowrelease.ArchiveTool{Version: "2.97.0", LinuxAMD64SHA256: strings.Repeat("d", 64)},
					Go:         workflowrelease.ArchiveTool{Version: "1.26.6", LinuxAMD64SHA256: strings.Repeat("e", 64)},
					NoMistakes: workflowrelease.NoMistakesTool{Version: "v1.41.2", Repository: "skyhuang233/no-mistakes", Commit: strings.Repeat("f", 40)},
				}},
				SBOM: workflowrelease.SBOM{Name: workflowrelease.SBOMAssetName, Format: "spdx-json", SHA256: strings.Repeat("3", 64), Scan: workflowrelease.Scan{Scanner: "grype", SeverityCutoff: "high", OnlyFixed: true}},
			}
			raw, err := manifest.Canonical()
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(manifestDirectory, workflowrelease.ManifestAssetName)
			if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			manifestDigest := sha256.Sum256(raw)
			if qualification {
				t.Setenv(setupQualificationEnvironment, "1")
				t.Setenv(candidateDirectoryEnvironment, manifestDirectory)
				t.Setenv(candidateVersionEnvironment, manifest.Version)
				t.Setenv(candidateSourceCommitEnvironment, sourceCommit)
			}
			engine := Engine{BundleRoot: bundle}
			request := Request{
				SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: manifest.Version,
				BundleDigest: "sha256:" + bundleDigest, GitHubOwner: "owner",
				VerifiedReleaseManifest: &VerifiedReleaseManifest{ManifestPath: manifestPath, ManifestSHA256: hex.EncodeToString(manifestDigest[:]), SourceCommit: sourceCommit},
			}
			request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			request, err = DecodeRequest(encoded)
			if err != nil {
				t.Fatal(err)
			}
			result, err := engine.Apply(context.Background(), request)
			if err != nil || result.Status != "ready" {
				t.Fatalf("apply = %#v, %v", result, err)
			}
			active, err := ReadActive(home)
			if err != nil {
				t.Fatal(err)
			}
			database, err := store.OpenForRuntime(context.Background(), filepath.Join(home, "platform", "generations", active.Generation, "workflow.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			release, err := database.ActiveWorkerRelease(context.Background())
			if err != nil || release.Version != manifest.Version || release.SourceCommit != sourceCommit || release.ImageReference != image || release.ManifestJSON != string(raw) {
				t.Fatalf("active Worker Release = %#v, %v", release, err)
			}
		})
	}
}

func TestApplyPersistsVerifiedPATInCandidateGenerationBeforeReady(t *testing.T) {
	home, digest := t.TempDir(), "sha256:"+strings.Repeat("a", 64)
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	engine := Engine{BundleRoot: bundle, VerifyPAT: func(context.Context, string, string) (githubcredential.Verification, error) {
		return githubcredential.Verification{FingerprintSHA256: credential.Fingerprint("secret"), Login: "owner", UserID: 7, Owner: "owner", Scopes: []string{"repo", "workflow"}, VerifiedAt: time.Now().UTC()}, nil
	}}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner", PAT: "secret"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	got, err := engine.Apply(context.Background(), request)
	if err != nil || got.Status != "ready" {
		t.Fatalf("apply=%#v,%v", got, err)
	}
	active, _ := ReadActive(home)
	db, err := store.Open(context.Background(), filepath.Join(home, "platform", "generations", active.Generation, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	value, err := db.GitHubPATVerification(context.Background())
	if err != nil || value.Owner != "owner" || value.FingerprintSHA256 != credential.Fingerprint("secret") {
		t.Fatalf("verification=%#v,%v", value, err)
	}
}

func TestSetupSkillFreshInspectAcceptanceBuildsExactLauncherApply(t *testing.T) {
	skill, err := os.ReadFile(filepath.Join("..", "..", "skills", "setup-agent-workflow", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(skill)
	if strings.Contains(text, "$inspection.evidence.capabilities") || !strings.Contains(text, "$inspection.evidence.required_capabilities") {
		t.Fatalf("Skill does not consume Launcher required_capabilities: %s", text)
	}
	if !strings.Contains(text, "$inspection.status -eq 'ready'") || !strings.Contains(text, "consent_id=$consentID") {
		t.Fatal("Skill does not build the reusable-consent branch")
	}

	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "fresh-home")
	digest := "sha256:" + strings.Repeat("a", 64)
	engine := Engine{BundleRoot: bundle}
	verified := writeTestVerifiedReleaseManifest(t, t.TempDir(), "0.0.1", digest)
	inspect := Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner", VerifiedReleaseManifest: verified}
	inspection, err := engine.Inspect(context.Background(), inspect)
	if err != nil {
		t.Fatal(err)
	}
	// Marshal and decode the actual Launcher response exactly as ConvertFrom-Json
	// exposes it to the documented PowerShell flow.
	rawInspection, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	var displayed struct {
		Status   string `json:"status"`
		Evidence struct {
			RequiredCapabilities []Capability `json:"required_capabilities"`
			ConsentID            string       `json:"consent_id"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(rawInspection, &displayed); err != nil {
		t.Fatal(err)
	}
	if displayed.Status != "consent_required" || len(displayed.Evidence.RequiredCapabilities) == 0 || displayed.Evidence.ConsentID != "" {
		t.Fatalf("fresh inspect JSON=%s", rawInspection)
	}
	// Model the user's acceptance: the returned capability array is forwarded
	// untouched into the strict request decoder and then into the real apply.
	apply := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner", AcceptedCapabilities: displayed.Evidence.RequiredCapabilities, VerifiedReleaseManifest: verified}
	rawApply, err := json.Marshal(apply)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRequest(rawApply)
	if err != nil {
		t.Fatalf("documented apply JSON rejected: %v\n%s", err, rawApply)
	}
	if !sameCapabilities(decoded.AcceptedCapabilities, displayed.Evidence.RequiredCapabilities) || decoded.ConsentID != "" {
		t.Fatalf("apply fields changed: %#v", decoded)
	}
	if result, err := engine.Apply(context.Background(), decoded); err != nil || result.Status != "ready" {
		t.Fatalf("apply=%#v,%v", result, err)
	}

	reusable, err := engine.Inspect(context.Background(), inspect)
	if err != nil || reusable.Status != "ready" {
		t.Fatalf("reusable inspect=%#v,%v", reusable, err)
	}
	rawReusable, err := json.Marshal(reusable)
	if err != nil {
		t.Fatal(err)
	}
	displayed = struct {
		Status   string `json:"status"`
		Evidence struct {
			RequiredCapabilities []Capability `json:"required_capabilities"`
			ConsentID            string       `json:"consent_id"`
		} `json:"evidence"`
	}{}
	if err := json.Unmarshal(rawReusable, &displayed); err != nil {
		t.Fatal(err)
	}
	if displayed.Evidence.ConsentID == "" || len(displayed.Evidence.RequiredCapabilities) != 0 {
		t.Fatalf("reusable inspect JSON=%s", rawReusable)
	}
	reuseApply := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner", ConsentID: displayed.Evidence.ConsentID, VerifiedReleaseManifest: verified}
	rawReuse, err := json.Marshal(reuseApply)
	if err != nil {
		t.Fatal(err)
	}
	decodedReuse, err := DecodeRequest(rawReuse)
	if err != nil || decodedReuse.ConsentID != displayed.Evidence.ConsentID || len(decodedReuse.AcceptedCapabilities) != 0 {
		t.Fatalf("reusable apply fields=%#v err=%v", decodedReuse, err)
	}
}

func TestFreshApplyRejectsInvalidTargetOrConsentWithoutCreatingWorkflowHome(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	engine := Engine{BundleRoot: bundle}
	home := filepath.Join(t.TempDir(), "missing-home")
	invalidTarget := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:invalid", GitHubOwner: "owner", AcceptedCapabilities: []Capability{{Name: "x", Value: "y"}}}
	if got, err := engine.Apply(context.Background(), invalidTarget); err != nil || got.Status != "blocked" {
		t.Fatalf("invalid target apply=%#v,%v", got, err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("invalid target created Workflow Home: %v", err)
	}

	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	for i := range request.AcceptedCapabilities {
		if request.AcceptedCapabilities[i].Name == "persist_plaintext_pat" {
			request.AcceptedCapabilities[i].Value = `{"path":"C:\\wrong\\github.pat","owner":"owner"}`
		}
	}
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "consent_required" {
		t.Fatalf("invalid consent apply=%#v,%v", got, err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("invalid consent created Workflow Home: %v", err)
	}
}

func TestFreshApplyRecordsConsentBeforeInstallationLock(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "fresh-home")
	engine := Engine{BundleRoot: bundle}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("b", 64), GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	called := false
	engine.AfterConsentRecorded = func(gotHome string) {
		called = true
		if gotHome != home {
			t.Fatalf("consent hook home=%q, want %q", gotHome, home)
		}
		if _, err := os.Stat(filepath.Join(home, "platform", "installation.lock")); !os.IsNotExist(err) {
			t.Fatalf("installation lock existed before consent was observed: %v", err)
		}
		entries, err := os.ReadDir(filepath.Join(home, "platform", "consents"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("consent was not the first durable record: entries=%v err=%v", entries, err)
		}
	}
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "ready" {
		t.Fatalf("fresh apply=%#v,%v", got, err)
	}
	if !called {
		t.Fatal("fresh apply never observed consent persistence")
	}
}

func TestFreshPrepareFailureReturnsRepairAndResumesSameConsentAttempt(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "fresh-home")
	lifecycle := &fakeLifecycle{prepareErr: errors.New("Docker Desktop did not start")}
	verify := func(_ context.Context, token, owner string) (githubcredential.Verification, error) {
		return githubcredential.Verification{FingerprintSHA256: credential.Fingerprint(token), Login: owner, UserID: 1, Owner: owner, Scopes: []string{"repo", "workflow"}, VerifiedAt: time.Now().UTC()}, nil
	}
	engine := Engine{BundleRoot: bundle, Lifecycle: lifecycle, VerifyPAT: verify}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner", PAT: "secret"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	first, err := engine.Apply(context.Background(), request)
	if err != nil || first.Status != "repair_required" {
		t.Fatalf("failed fresh apply=%#v,%v", first, err)
	}
	attemptFiles, err := os.ReadDir(filepath.Join(home, "platform", "attempts"))
	if err != nil || len(attemptFiles) != 1 {
		t.Fatalf("fresh failure did not persist one attempt: %v,%v", attemptFiles, err)
	}
	attemptID := strings.TrimSuffix(attemptFiles[0].Name(), ".json")
	inspect, err := engine.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
	if err != nil || inspect.Status != "ready" || inspect.Evidence["consent_id"] == nil {
		t.Fatalf("fresh repair inspect=%#v,%v", inspect, err)
	}
	lifecycle.prepareErr = nil
	retry := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: inspect.Evidence["consent_id"].(string), PAT: "secret"}
	ready, err := engine.Apply(context.Background(), retry)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("fresh repair=%#v,%v", ready, err)
	}
	active, err := ReadActive(home)
	if err != nil || active.AttemptID != attemptID {
		t.Fatalf("fresh repair replaced attempt: %#v,%v", active, err)
	}
}

func TestFreshRecoveryRejectsUnknownOrForeignState(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "fresh-home")
	engine := Engine{BundleRoot: bundle, Lifecycle: &fakeLifecycle{prepareErr: errors.New("prepare failed")}, VerifyPAT: func(_ context.Context, token, owner string) (githubcredential.Verification, error) {
		return githubcredential.Verification{FingerprintSHA256: credential.Fingerprint(token), Login: owner, UserID: 1, Owner: owner, Scopes: []string{"repo", "workflow"}, VerifiedAt: time.Now().UTC()}, nil
	}}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("b", 64), GitHubOwner: "owner", PAT: "secret"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "repair_required" {
		t.Fatalf("failed fresh=%#v,%v", got, err)
	}
	inspection, err := engine.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
	if err != nil || inspection.Status != "ready" {
		t.Fatalf("exact recovery inspection=%#v,%v", inspection, err)
	}
	consentID := inspection.Evidence["consent_id"].(string)
	if err := os.WriteFile(filepath.Join(home, "unexpected.txt"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err = engine.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
	if err != nil || inspection.Status != "consent_required" {
		t.Fatalf("unknown state became reusable: %#v,%v", inspection, err)
	}
	retry := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: consentID, PAT: "secret"}
	if got, err := engine.Apply(context.Background(), retry); err != nil || got.Status != "blocked" {
		t.Fatalf("unknown state apply=%#v,%v", got, err)
	}
	if err := os.Remove(filepath.Join(home, "unexpected.txt")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(home, "platform", "consents", "foreign.json"), Consent{SchemaVersion: ProtocolVersion, ID: "foreign", TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(home, "platform", "attempts", "foreign.json"), Attempt{SchemaVersion: ProtocolVersion, ID: "foreign", TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, Generation: "foreign", ConsentID: "foreign"}); err != nil {
		t.Fatal(err)
	}
	inspection, err = engine.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
	if err != nil || inspection.Status != "consent_required" {
		t.Fatalf("foreign consent or attempt became reusable: %#v,%v", inspection, err)
	}
}

func TestConsentOnlyRecoveryRejectsUnsafeStateLayout(t *testing.T) {
	for _, test := range []struct {
		name    string
		arrange func(*testing.T, string, string)
	}{
		{
			name: "regular state file",
			arrange: func(t *testing.T, home, _ string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(home, "state"), []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "state symlink",
			arrange: func(t *testing.T, home, outside string) {
				t.Helper()
				if err := os.Symlink(outside, filepath.Join(home, "state")); err != nil {
					t.Skipf("directory symlinks are unavailable: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := t.TempDir()
			writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
			home := filepath.Join(t.TempDir(), "fresh-home")
			request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner"}
			crashing := Engine{BundleRoot: bundle, AfterConsentWritten: func(string) { panic("simulated consent-only crash") }}
			request.AcceptedCapabilities = requiredCapabilities(t, crashing, request)
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("consent-only crash was not injected")
					}
				}()
				_, _ = crashing.Apply(context.Background(), request)
			}()
			consentID := readOnlyConsentID(t, home)
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("outside remains untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			test.arrange(t, home, outside)

			inspectRequest := Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"}
			if got, err := (Engine{BundleRoot: bundle}).Inspect(context.Background(), inspectRequest); err != nil || got.Status != "consent_required" {
				t.Fatalf("unsafe state recovery inspect=%#v, %v", got, err)
			}
			if got, err := (Engine{BundleRoot: bundle}).Apply(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: consentID}); err != nil || got.Status != "blocked" {
				t.Fatalf("unsafe state recovery apply=%#v, %v", got, err)
			}
			if contents, err := os.ReadDir(outside); err != nil || len(contents) != 1 || contents[0].Name() != "sentinel" {
				t.Fatalf("unsafe state recovery changed outside target=%v, %v", contents, err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside remains untouched" {
				t.Fatalf("outside sentinel=%q, %v", data, err)
			}
		})
	}
}

func TestFreshRecoveryRevalidatesUnderLockBeforeAnyMutation(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "fresh-home")
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner", PAT: "secret"}
	initial := Engine{BundleRoot: bundle, VerifyPAT: verifiedTestPAT, ReconcilePath: func(string) error { return errors.New("PATH denied") }}
	request.AcceptedCapabilities = requiredCapabilities(t, initial, request)
	if got, err := initial.Apply(context.Background(), request); err != nil || got.Status != "repair_required" {
		t.Fatalf("initial repair state=%#v, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "platform", "attempts"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("initial attempt=%v, %v", entries, err)
	}
	attemptID := strings.TrimSuffix(entries[0].Name(), ".json")
	foreign := filepath.Join(home, "foreign-state.txt")
	lifecycle := &fakeLifecycle{}
	verificationCalls := 0
	retry := Engine{
		BundleRoot: bundle,
		Lifecycle:  lifecycle,
		VerifyPAT: func(ctx context.Context, token, owner string) (githubcredential.Verification, error) {
			verificationCalls++
			return verifiedTestPAT(ctx, token, owner)
		},
		BeforeRecoveryLock: func(string) {
			if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	if got, err := retry.Apply(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: readOnlyConsentID(t, home), PAT: "secret"}); err != nil || got.Status != "blocked" {
		t.Fatalf("TOCTOU recovery apply=%#v, %v", got, err)
	}
	if verificationCalls != 0 || len(lifecycle.calls) != 0 {
		t.Fatalf("TOCTOU reached credential or dependency preparation: verification=%d lifecycle=%v", verificationCalls, lifecycle.calls)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign" {
		t.Fatalf("TOCTOU recovery removed foreign state: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, "platform", "generations", generationName(request.BundleDigest))); !os.IsNotExist(err) {
		t.Fatalf("TOCTOU recovery staged generation: %v", err)
	}
	attempt, err := readAttempt(home, attemptID)
	if err != nil || attempt.ID != attemptID || attempt.Phase != "failed" {
		t.Fatalf("TOCTOU recovery changed retained attempt=%#v, %v", attempt, err)
	}
}

func readOnlyConsentID(t *testing.T, home string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "platform", "consents"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("recovery consent=%v, %v", entries, err)
	}
	return strings.TrimSuffix(entries[0].Name(), ".json")
}

func TestConsentAttemptCrashWindowResumesWithExactConsent(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "fresh-home")
	verify := func(_ context.Context, token, owner string) (githubcredential.Verification, error) {
		return githubcredential.Verification{FingerprintSHA256: credential.Fingerprint(token), Login: owner, UserID: 1, Owner: owner, Scopes: []string{"repo", "workflow"}, VerifiedAt: time.Now().UTC()}, nil
	}
	crashing := Engine{BundleRoot: bundle, VerifyPAT: verify, AfterConsentWritten: func(string) { panic("simulated process crash") }}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("c", 64), GitHubOwner: "owner", PAT: "secret"}
	request.AcceptedCapabilities = requiredCapabilities(t, crashing, request)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("consent hook did not simulate a process crash")
			}
		}()
		_, _ = crashing.Apply(context.Background(), request)
	}()
	inspect, err := Engine{BundleRoot: bundle, VerifyPAT: verify}.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
	if err != nil || inspect.Status != "ready" {
		t.Fatalf("consent crash was not reusable: %#v,%v", inspect, err)
	}
	retry := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: inspect.Evidence["consent_id"].(string), PAT: "secret"}
	if got, err := (Engine{BundleRoot: bundle, VerifyPAT: verify}).Apply(context.Background(), retry); err != nil || got.Status != "ready" {
		t.Fatalf("consent crash resume=%#v,%v", got, err)
	}
}

func TestFreshInitialPATRejectionRecoversSameConsentAttempt(t *testing.T) {
	bundle, home := t.TempDir(), filepath.Join(t.TempDir(), "fresh-home")
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	fail := true
	verify := func(ctx context.Context, token, owner string) (githubcredential.Verification, error) {
		if fail {
			return githubcredential.Verification{}, errors.New("PAT rejected")
		}
		return verifiedTestPAT(ctx, token, owner)
	}
	engine := Engine{BundleRoot: bundle, VerifyPAT: verify}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("f", 64), GitHubOwner: "owner", PAT: "bad"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "repair_required" {
		t.Fatalf("initial PAT=%#v,%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, "state")); !os.IsNotExist(err) {
		t.Fatalf("PAT rejection created credential state: %v", err)
	}
	inspect, err := engine.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
	if err != nil || inspect.Status != "ready" {
		t.Fatalf("PAT recovery inspect=%#v,%v", inspect, err)
	}
	entries, _ := os.ReadDir(filepath.Join(home, "platform", "attempts"))
	attemptID := strings.TrimSuffix(entries[0].Name(), ".json")
	fail = false
	if got, err := engine.Apply(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: inspect.Evidence["consent_id"].(string), PAT: "good"}); err != nil || got.Status != "ready" {
		t.Fatalf("corrected PAT=%#v,%v", got, err)
	}
	active, err := ReadActive(home)
	if err != nil || active.AttemptID != attemptID {
		t.Fatalf("PAT retry replaced attempt=%#v,%v", active, err)
	}
}

func TestFreshPreActivationArtifactsAreCleanedAndSameAttemptResumes(t *testing.T) {
	for _, test := range []struct {
		name   string
		engine func(string) Engine
	}{
		{
			name: "dispatcher copied before PATH reconciliation failure",
			engine: func(bundle string) Engine {
				return Engine{BundleRoot: bundle, ReconcilePath: func(string) error { return errors.New("PATH denied") }, VerifyPAT: verifiedTestPAT}
			},
		},
		{
			name: "candidate PAT verification failure",
			engine: func(bundle string) Engine {
				calls := 0
				return Engine{BundleRoot: bundle, VerifyPAT: func(ctx context.Context, token, owner string) (githubcredential.Verification, error) {
					calls++
					if calls == 2 {
						return githubcredential.Verification{}, errors.New("candidate verification failed")
					}
					return verifiedTestPAT(ctx, token, owner)
				}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := t.TempDir()
			writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
			home := filepath.Join(t.TempDir(), "fresh-home")
			engine := test.engine(bundle)
			request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("d", 64), GitHubOwner: "owner", PAT: "secret"}
			request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
			first, err := engine.Apply(context.Background(), request)
			if err != nil || first.Status != "repair_required" {
				t.Fatalf("failed fresh=%#v,%v", first, err)
			}
			if _, err := os.Stat(filepath.Join(home, "bin", "workflow.exe")); !os.IsNotExist(err) {
				t.Fatalf("pre-activation dispatcher was retained: %v", err)
			}
			generation := generationName(request.BundleDigest)
			if _, err := os.Stat(filepath.Join(home, "platform", "generations", generation)); !os.IsNotExist(err) {
				t.Fatalf("pre-activation generation was retained: %v", err)
			}
			attempts, err := os.ReadDir(filepath.Join(home, "platform", "attempts"))
			if err != nil || len(attempts) != 1 {
				t.Fatalf("attempt evidence=%v,%v", attempts, err)
			}
			attemptID := strings.TrimSuffix(attempts[0].Name(), ".json")
			inspect, err := engine.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
			if err != nil || inspect.Status != "ready" {
				t.Fatalf("recovery inspect=%#v,%v", inspect, err)
			}
			retry := Engine{BundleRoot: bundle, VerifyPAT: verifiedTestPAT}
			result, err := retry.Apply(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: inspect.Evidence["consent_id"].(string), PAT: "secret"})
			if err != nil || result.Status != "ready" {
				t.Fatalf("recovery apply=%#v,%v", result, err)
			}
			active, err := ReadActive(home)
			if err != nil || active.AttemptID != attemptID {
				t.Fatalf("recovery replaced attempt: %#v,%v", active, err)
			}
		})
	}
}

func TestFreshPartialStageFailureCleansGenerationAndResumesSameAttempt(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	if err := os.RemoveAll(filepath.Join(bundle, "skills")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "fresh-home")
	engine := Engine{BundleRoot: bundle, VerifyPAT: verifiedTestPAT}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("e", 64), GitHubOwner: "owner", PAT: "secret"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "repair_required" {
		t.Fatalf("partial stage=%#v,%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, "platform", "generations", generationName(request.BundleDigest))); !os.IsNotExist(err) {
		t.Fatalf("partial generation survived stage failure: %v", err)
	}
	inspect, err := engine.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, GitHubOwner: "owner"})
	if err != nil || inspect.Status != "ready" {
		t.Fatalf("partial stage inspect=%#v,%v", inspect, err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "skills", "agent-workflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "skills", "agent-workflow", "SKILL.md"), []byte("skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := engine.Apply(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: request.TargetVersion, BundleDigest: request.BundleDigest, ConsentID: inspect.Evidence["consent_id"].(string), PAT: "secret"}); err != nil || got.Status != "ready" {
		t.Fatalf("partial stage retry=%#v,%v", got, err)
	}
}

func verifiedTestPAT(_ context.Context, token, owner string) (githubcredential.Verification, error) {
	return githubcredential.Verification{FingerprintSHA256: credential.Fingerprint(token), Login: owner, UserID: 1, Owner: owner, Scopes: []string{"repo", "workflow"}, VerifiedAt: time.Now().UTC()}, nil
}

func TestConcurrentFreshApplyHasOneProspectiveLockOwner(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "fresh-home")
	engine := Engine{BundleRoot: bundle}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("c", 64), GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	engine.AfterConsentRecorded = func(string) {
		once.Do(func() { close(entered) })
		<-release
	}
	first := make(chan Result, 1)
	go func() {
		got, err := engine.Apply(context.Background(), request)
		if err != nil {
			t.Errorf("first apply: %v", err)
		}
		first <- got
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first fresh apply did not retain prospective lock")
	}
	second, err := engine.Apply(context.Background(), request)
	if err != nil || second.Status != "blocked" {
		t.Fatalf("second fresh apply=%#v,%v, want locked", second, err)
	}
	close(release)
	select {
	case got := <-first:
		if got.Status != "ready" {
			t.Fatalf("first fresh apply=%#v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first fresh apply did not finish")
	}
	for _, directory := range []string{"consents", "attempts"} {
		entries, err := os.ReadDir(filepath.Join(home, "platform", directory))
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s entries=%v err=%v, want one mutation owner", directory, entries, err)
		}
	}
}

type fakeLifecycle struct {
	calls      []string
	failReady  bool
	prepareErr error
}

type upgradeOrderLifecycle struct {
	t            *testing.T
	home         string
	targetDigest string
	calls        []string
}

func (f *upgradeOrderLifecycle) Prepare(context.Context, Request, Consent) error {
	if len(f.calls) != 1 || f.calls[0] != "stop" {
		f.t.Fatalf("dependency preparation order=%v", f.calls)
	}
	f.calls = append(f.calls, "prepare")
	return nil
}
func (f *upgradeOrderLifecycle) Stop(_ context.Context, home string, active Active) error {
	if home != f.home {
		f.t.Fatalf("stop home=%q", home)
	}
	entries, err := os.ReadDir(filepath.Join(home, "platform", "consents"))
	if err != nil || len(entries) == 0 {
		f.t.Fatalf("stop ran before consent was recorded: entries=%v err=%v", entries, err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(home, "platform", "generations", active.Generation, "workflow.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer raw.Close()
	var fences int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM platform_maintenance_fences WHERE bundle_digest=?`, f.targetDigest).Scan(&fences); err != nil || fences != 1 {
		f.t.Fatalf("stop ran without target fence: fences=%d err=%v", fences, err)
	}
	f.calls = append(f.calls, "stop")
	return nil
}
func (f *upgradeOrderLifecycle) Start(_ context.Context, home string, active Active) error {
	if len(f.calls) != 2 || f.calls[1] != "prepare" {
		f.t.Fatalf("candidate start order=%v", f.calls)
	}
	if _, err := os.Stat(filepath.Join(home, "platform", "generations", active.Generation, "workflow.exe")); err != nil {
		f.t.Fatalf("candidate was not staged before start: %v", err)
	}
	f.calls = append(f.calls, "start")
	return nil
}
func (f *upgradeOrderLifecycle) Ready(context.Context, string, Active) error {
	f.calls = append(f.calls, "ready")
	return nil
}

func (f *fakeLifecycle) Prepare(context.Context, Request, Consent) error {
	f.calls = append(f.calls, "prepare")
	return f.prepareErr
}
func (f *fakeLifecycle) Stop(context.Context, string, Active) error {
	f.calls = append(f.calls, "stop")
	return nil
}
func (f *fakeLifecycle) Start(context.Context, string, Active) error {
	f.calls = append(f.calls, "start")
	return nil
}
func (f *fakeLifecycle) Ready(context.Context, string, Active) error {
	f.calls = append(f.calls, "ready")
	if f.failReady {
		return os.ErrNotExist
	}
	return nil
}
func (f *fakeLifecycle) Restart(context.Context, string, Active) error {
	f.calls = append(f.calls, "restart")
	return nil
}

func TestUpgradeRejectedPATPreservesOldCredentialAndRestartsActiveGeneration(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	firstBundle := t.TempDir()
	writeTestBundle(t, firstBundle, map[string]string{"platform/workflow.exe": "one", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	first := Engine{BundleRoot: firstBundle, VerifyPAT: func(_ context.Context, token, owner string) (githubcredential.Verification, error) {
		return githubcredential.Verification{FingerprintSHA256: credential.Fingerprint(token), Owner: owner, Login: owner, UserID: 1, Scopes: []string{"repo", "workflow"}, VerifiedAt: time.Now().UTC()}, nil
	}}
	initial := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner", PAT: "old-pat"}
	initial.AcceptedCapabilities = requiredCapabilities(t, first, initial)
	if got, err := first.Apply(context.Background(), initial); err != nil || got.Status != "ready" {
		t.Fatalf("initial=%#v,%v", got, err)
	}
	oldBytes, err := os.ReadFile(filepath.Join(home, "state", "credentials", "github.pat"))
	if err != nil {
		t.Fatal(err)
	}
	oldActive, err := ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	secondBundle := t.TempDir()
	writeTestBundleVersion(t, secondBundle, "0.0.2", map[string]string{"platform/workflow.exe": "two", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	lifecycle := &fakeLifecycle{}
	second := Engine{BundleRoot: secondBundle, Lifecycle: lifecycle, VerifyPAT: func(context.Context, string, string) (githubcredential.Verification, error) {
		return githubcredential.Verification{}, errors.New("PAT rejected")
	}}
	upgrade := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.2", BundleDigest: "sha256:" + strings.Repeat("b", 64), GitHubOwner: "owner", PAT: "bad-pat"}
	upgrade.AcceptedCapabilities = requiredCapabilities(t, second, upgrade)
	if got, err := second.Apply(context.Background(), upgrade); err != nil || got.Status != "blocked" {
		t.Fatalf("rejected upgrade=%#v,%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(home, "state", "credentials", "github.pat")); err != nil || string(got) != string(oldBytes) {
		t.Fatalf("old PAT changed: %q,%v", got, err)
	}
	after, err := ReadActive(home)
	if err != nil || after.Generation != oldActive.Generation || after.Readiness != activeReady {
		t.Fatalf("old active was not preserved: %#v,%v", after, err)
	}
	if strings.Join(lifecycle.calls, ",") != "stop,restart" {
		t.Fatalf("old lifecycle was not restarted: %v", lifecycle.calls)
	}
	// A valid rotated PAT is also restored when a later pre-activation stage
	// fails, rather than only when pre-verification rejects it.
	brokenBundle := t.TempDir()
	writeTestBundleVersion(t, brokenBundle, "0.0.3", map[string]string{"platform/workflow.exe": "three", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	brokenLifecycle := &fakeLifecycle{}
	broken := Engine{BundleRoot: brokenBundle, Lifecycle: brokenLifecycle, VerifyPAT: func(_ context.Context, token, owner string) (githubcredential.Verification, error) {
		return githubcredential.Verification{FingerprintSHA256: credential.Fingerprint(token), Owner: owner, Login: owner, UserID: 1, Scopes: []string{"repo", "workflow"}, VerifiedAt: time.Now().UTC()}, nil
	}}
	stagedFailure := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.3", BundleDigest: "sha256:" + strings.Repeat("c", 64), GitHubOwner: "owner", PAT: "rotated-pat"}
	stagedFailure.AcceptedCapabilities = requiredCapabilities(t, broken, stagedFailure)
	if err := os.RemoveAll(filepath.Join(brokenBundle, "skills")); err != nil {
		t.Fatal(err)
	}
	if got, err := broken.Apply(context.Background(), stagedFailure); err != nil || got.Status != "blocked" {
		t.Fatalf("staging failure=%#v,%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(home, "state", "credentials", "github.pat")); err != nil || string(got) != string(oldBytes) {
		t.Fatalf("stage failure changed old PAT: %q,%v", got, err)
	}
	if after, err := ReadActive(home); err != nil || after.Generation != oldActive.Generation || after.Readiness != activeReady {
		t.Fatalf("stage failure changed active: %#v,%v", after, err)
	}
	if strings.Join(brokenLifecycle.calls, ",") != "stop,prepare,restart" {
		t.Fatalf("stage failure did not restart old lifecycle: %v", brokenLifecycle.calls)
	}
}

func TestApplyCommitsRepairRequiredBeforeReadyAndCreatesGeneration(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{
		"platform/workflow.exe":               "versioned cli",
		"setup/workflow-setup.exe":            "launcher",
		"skills/agent-workflow/SKILL.md":      "skill",
		"repository-contract/repository.json": "contract",
	})
	home := filepath.Join(t.TempDir(), "home")
	digest := "sha256:" + strings.Repeat("a", 64)
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner"}
	engine := Engine{BundleRoot: bundle}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	result, err := engine.Apply(context.Background(), request)
	if err != nil || result.Status != "ready" {
		t.Fatalf("Apply() = %#v, %v", result, err)
	}
	active, err := ReadActive(home)
	if err != nil || active.Readiness != activeReady || active.Generation != strings.Repeat("a", 64) {
		t.Fatalf("active = %#v, %v", active, err)
	}
	if _, err := os.Stat(filepath.Join(home, "platform", "generations", active.Generation, "workflow.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "credentials", "github.pat")); !os.IsNotExist(err) {
		t.Fatalf("no PAT supplied; credential file error = %v", err)
	}
}

func TestVerifyRequiresReadyActiveRecord(t *testing.T) {
	home := t.TempDir()
	if err := writeJSONAtomic(activePath(home), Active{SchemaVersion: ProtocolVersion, Generation: "a", Version: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), AttemptID: "attempt", ConsentID: "consent", Readiness: activeRepairRequired}); err != nil {
		t.Fatal(err)
	}
	got, err := (Engine{}).Verify(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Verify, WorkflowHome: home})
	if err != nil || got.Status != "repair_required" {
		t.Fatalf("Verify() = %#v, %v", got, err)
	}
}

func TestUpgradeAndRepairKeepGenerationsAndUseExactActiveTarget(t *testing.T) {
	makeBundle := func(t *testing.T, marker, version string) string {
		t.Helper()
		root := t.TempDir()
		writeTestBundleVersion(t, root, version, map[string]string{"platform/workflow.exe": marker, "setup/workflow-setup.exe": "launcher", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
		return root
	}
	home := filepath.Join(t.TempDir(), "home")
	firstDigest := "sha256:" + strings.Repeat("a", 64)
	first := Engine{BundleRoot: makeBundle(t, "one", "0.0.1")}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: firstDigest, GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, first, request)
	if got, err := first.Apply(context.Background(), request); err != nil || got.Status != "ready" {
		t.Fatalf("fresh=%#v,%v", got, err)
	}
	secondDigest := "sha256:" + strings.Repeat("b", 64)
	second := Engine{BundleRoot: makeBundle(t, "two", "0.0.2")}
	upgrade := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.2", BundleDigest: secondDigest, GitHubOwner: "owner"}
	upgrade.AcceptedCapabilities = requiredCapabilities(t, second, upgrade)
	if got, err := second.Apply(context.Background(), upgrade); err != nil || got.Status != "ready" {
		t.Fatalf("upgrade=%#v,%v", got, err)
	}
	active, err := ReadActive(home)
	if err != nil || active.Version != "0.0.2" || active.Generation != strings.Repeat("b", 64) {
		t.Fatalf("active=%#v,%v", active, err)
	}
	if _, err := os.Stat(filepath.Join(home, "platform", "generations", strings.Repeat("a", 64), "workflow.db")); err != nil {
		t.Fatalf("prior generation was not retained: %v", err)
	}
	inspect, err := second.Inspect(context.Background(), Request{SchemaVersion: ProtocolVersion, Operation: Inspect, WorkflowHome: home, Purpose: PurposeTargetState, TargetVersion: "0.0.2", BundleDigest: secondDigest, GitHubOwner: "owner"})
	if err != nil || inspect.Status != "ready" {
		t.Fatalf("exact repair inspect=%#v,%v", inspect, err)
	}
}

func TestUpgradeActiveWorkReturnsBeforeAnyInstallationMutation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	firstDigest := "sha256:" + strings.Repeat("a", 64)
	firstBundle := t.TempDir()
	writeTestBundle(t, firstBundle, map[string]string{"platform/workflow.exe": "first", "setup/workflow-setup.exe": "first-setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	first := Engine{BundleRoot: firstBundle}
	fresh := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: firstDigest, GitHubOwner: "owner"}
	fresh.AcceptedCapabilities = requiredCapabilities(t, first, fresh)
	if result, err := first.Apply(context.Background(), fresh); err != nil || result.Status != "ready" {
		t.Fatalf("fresh=%#v,%v", result, err)
	}
	active, err := ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	seedActiveWorkerRun(t, filepath.Join(home, "platform", "generations", active.Generation, "workflow.db"))
	beforeDispatcher, err := os.ReadFile(filepath.Join(home, "bin", "workflow.exe"))
	if err != nil {
		t.Fatal(err)
	}
	beforeConsents, err := os.ReadDir(filepath.Join(home, "platform", "consents"))
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempts, err := os.ReadDir(filepath.Join(home, "platform", "attempts"))
	if err != nil {
		t.Fatal(err)
	}
	secondDigest := "sha256:" + strings.Repeat("b", 64)
	secondBundle := t.TempDir()
	writeTestBundleVersion(t, secondBundle, "0.0.2", map[string]string{"platform/workflow.exe": "second", "setup/workflow-setup.exe": "second-setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	lifecycle := &fakeLifecycle{}
	pathCalls, patCalls := 0, 0
	second := Engine{BundleRoot: secondBundle, Lifecycle: lifecycle, ReconcilePath: func(string) error { pathCalls++; return nil }, VerifyPAT: func(context.Context, string, string) (githubcredential.Verification, error) {
		patCalls++
		return githubcredential.Verification{}, nil
	}}
	upgrade := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.2", BundleDigest: secondDigest, GitHubOwner: "owner", PAT: "secret"}
	upgrade.AcceptedCapabilities = requiredCapabilities(t, second, upgrade)
	result, err := second.Apply(context.Background(), upgrade)
	if err != nil || result.Status != "active_work" {
		t.Fatalf("active-work upgrade=%#v,%v", result, err)
	}
	if len(lifecycle.calls) != 0 || pathCalls != 0 || patCalls != 0 {
		t.Fatalf("active work performed lifecycle/PATH/PAT mutations: lifecycle=%v path=%d pat=%d", lifecycle.calls, pathCalls, patCalls)
	}
	if _, err := os.Stat(filepath.Join(home, "platform", "generations", strings.Repeat("b", 64))); !os.IsNotExist(err) {
		t.Fatalf("active work staged target generation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "credentials", "github.pat")); !os.IsNotExist(err) {
		t.Fatalf("active work wrote PAT: %v", err)
	}
	afterDispatcher, err := os.ReadFile(filepath.Join(home, "bin", "workflow.exe"))
	if err != nil || string(afterDispatcher) != string(beforeDispatcher) {
		t.Fatalf("active work replaced dispatcher: err=%v", err)
	}
	afterConsents, _ := os.ReadDir(filepath.Join(home, "platform", "consents"))
	afterAttempts, _ := os.ReadDir(filepath.Join(home, "platform", "attempts"))
	if len(afterConsents) != len(beforeConsents) || len(afterAttempts) != len(beforeAttempts) {
		t.Fatalf("active work wrote consent/attempt: consents %d->%d attempts %d->%d", len(beforeConsents), len(afterConsents), len(beforeAttempts), len(afterAttempts))
	}
}

func TestUpgradeRejectsFutureOrMaintenanceIncompatibleActiveGenerationBeforeFenceOrCandidateMutation(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "newer",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				raw, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				if _, err := raw.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, 'future')`, store.LatestSchemaVersion+1); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing maintenance fence contract",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				raw, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				if _, err := raw.Exec(`DROP TABLE platform_maintenance_fences`); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			firstBundle := t.TempDir()
			writeTestBundle(t, firstBundle, map[string]string{"platform/workflow.exe": "first", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
			first := Engine{BundleRoot: firstBundle}
			fresh := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner"}
			fresh.AcceptedCapabilities = requiredCapabilities(t, first, fresh)
			if result, err := first.Apply(ctx, fresh); err != nil || result.Status != "ready" {
				t.Fatalf("fresh=%#v,%v", result, err)
			}
			active, err := ReadActive(home)
			if err != nil {
				t.Fatal(err)
			}
			activeDB := filepath.Join(home, "platform", "generations", active.Generation, "workflow.db")
			test.mutate(t, activeDB)
			before, err := os.ReadFile(activeDB)
			if err != nil {
				t.Fatal(err)
			}
			secondBundle := t.TempDir()
			writeTestBundleVersion(t, secondBundle, "0.0.2", map[string]string{"platform/workflow.exe": "second", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
			lifecycle := &fakeLifecycle{}
			second := Engine{BundleRoot: secondBundle, Lifecycle: lifecycle}
			upgrade := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.2", BundleDigest: "sha256:" + strings.Repeat("b", 64), GitHubOwner: "owner"}
			upgrade.AcceptedCapabilities = requiredCapabilities(t, second, upgrade)
			result, err := second.Apply(ctx, upgrade)
			if err != nil || result.Status != "blocked" {
				t.Fatalf("upgrade=%#v,%v", result, err)
			}
			after, err := os.ReadFile(activeDB)
			if err != nil || string(after) != string(before) {
				t.Fatalf("upgrade changed mismatched active database: %v", err)
			}
			if len(lifecycle.calls) != 0 {
				t.Fatalf("schema mismatch crossed fence into lifecycle: %v", lifecycle.calls)
			}
			if _, err := os.Stat(filepath.Join(home, "platform", "generations", strings.Repeat("b", 64))); !os.IsNotExist(err) {
				t.Fatalf("schema mismatch staged candidate: %v", err)
			}
		})
	}
}

func TestUpgradeFencesAndMigratesCandidateFromOlderMaintenanceCompatibleGeneration(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), "home")
	firstBundle := t.TempDir()
	writeTestBundle(t, firstBundle, map[string]string{"platform/workflow.exe": "first", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	first := Engine{BundleRoot: firstBundle}
	fresh := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("a", 64), GitHubOwner: "owner"}
	fresh.AcceptedCapabilities = requiredCapabilities(t, first, fresh)
	if result, err := first.Apply(ctx, fresh); err != nil || result.Status != "ready" {
		t.Fatalf("fresh=%#v,%v", result, err)
	}
	old, err := ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	oldDatabase := filepath.Join(home, "platform", "generations", old.Generation, "workflow.db")
	raw, err := sql.Open("sqlite", oldDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM schema_migrations WHERE version = ?; PRAGMA wal_checkpoint(TRUNCATE)`, store.LatestSchemaVersion); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	secondBundle := t.TempDir()
	writeTestBundleVersion(t, secondBundle, "0.0.2", map[string]string{"platform/workflow.exe": "second", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	lifecycle := &fakeLifecycle{}
	second := Engine{BundleRoot: secondBundle, Lifecycle: lifecycle}
	upgrade := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.2", BundleDigest: "sha256:" + strings.Repeat("b", 64), GitHubOwner: "owner"}
	upgrade.AcceptedCapabilities = requiredCapabilities(t, second, upgrade)
	if result, err := second.Apply(ctx, upgrade); err != nil || result.Status != "ready" {
		t.Fatalf("upgrade=%#v,%v", result, err)
	}
	if got := strings.Join(lifecycle.calls, ","); got != "stop,prepare,start,ready" {
		t.Fatalf("maintenance lifecycle=%q", got)
	}
	assertSchemaVersion := func(path string, want int) {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var got int
		if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&got); err != nil || got != want {
			t.Fatalf("schema %q=%d,%v want %d", path, got, err, want)
		}
	}
	assertSchemaVersion(oldDatabase, store.LatestSchemaVersion-1)
	active, err := ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaVersion(filepath.Join(home, "platform", "generations", active.Generation, "workflow.db"), store.LatestSchemaVersion)
	backups, err := filepath.Glob(filepath.Join(home, "backups", "*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("maintenance backup=%v,%v", backups, err)
	}
}

func TestUpgradeOrdersFenceConsentStopDependenciesAndStage(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	firstDigest := "sha256:" + strings.Repeat("c", 64)
	firstBundle := t.TempDir()
	writeTestBundle(t, firstBundle, map[string]string{"platform/workflow.exe": "first", "setup/workflow-setup.exe": "first-setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	first := Engine{BundleRoot: firstBundle}
	fresh := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: firstDigest, GitHubOwner: "owner"}
	fresh.AcceptedCapabilities = requiredCapabilities(t, first, fresh)
	if result, err := first.Apply(context.Background(), fresh); err != nil || result.Status != "ready" {
		t.Fatalf("fresh=%#v,%v", result, err)
	}
	secondDigest := "sha256:" + strings.Repeat("d", 64)
	secondBundle := t.TempDir()
	writeTestBundleVersion(t, secondBundle, "0.0.2", map[string]string{"platform/workflow.exe": "second", "setup/workflow-setup.exe": "second-setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	lifecycle := &upgradeOrderLifecycle{t: t, home: home, targetDigest: secondDigest}
	second := Engine{BundleRoot: secondBundle, Lifecycle: lifecycle}
	upgrade := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.2", BundleDigest: secondDigest, GitHubOwner: "owner"}
	upgrade.AcceptedCapabilities = requiredCapabilities(t, second, upgrade)
	if result, err := second.Apply(context.Background(), upgrade); err != nil || result.Status != "ready" {
		t.Fatalf("ordered upgrade=%#v,%v", result, err)
	}
	if got := strings.Join(lifecycle.calls, ","); got != "stop,prepare,start,ready" {
		t.Fatalf("lifecycle order=%q", got)
	}
}

func seedActiveWorkerRun(t *testing.T, databasePath string) {
	t.Helper()
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := raw.Exec(`INSERT INTO worker_runs(run_id,session_id,attempt,lease_generation,state,started_at) VALUES('active-run','active-session',1,1,'running',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO run_leases(lease_token,run_id,session_id,generation,state,expires_at,created_at) VALUES('active-lease','active-run','active-session',1,'active',?,?)`, expires, now); err != nil {
		t.Fatal(err)
	}
}

func TestRepairReusesOnlyMatchingConsentAndAttempt(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "launcher", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "home")
	digest := "sha256:" + strings.Repeat("c", 64)
	engine := Engine{BundleRoot: bundle}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	if _, err := engine.Apply(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	active, err := ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	original := active.AttemptID
	active.Readiness = activeRepairRequired
	if err := writeJSONAtomic(activePath(home), active); err != nil {
		t.Fatal(err)
	}
	request.ConsentID = active.ConsentID
	request.AcceptedCapabilities = nil
	request.GitHubOwner = ""
	if got, err := engine.Apply(context.Background(), request); err != nil || got.Status != "ready" {
		t.Fatalf("repair=%#v,%v", got, err)
	}
	after, err := ReadActive(home)
	if err != nil || after.AttemptID != original {
		t.Fatalf("attempt was not resumed: %#v,%v", after, err)
	}
}

func TestActiveRepairKeepsOpenGenerationDatabaseAndAttempt(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "home")
	lifecycle := &fakeLifecycle{failReady: true}
	engine := Engine{BundleRoot: bundle, Lifecycle: lifecycle}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("f", 64), GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	if result, err := engine.Apply(context.Background(), request); err != nil || result.Status != "repair_required" {
		t.Fatalf("initial repair state=%#v,%v", result, err)
	}
	active, err := ReadActive(home)
	if err != nil {
		t.Fatal(err)
	}
	originalAttempt := active.AttemptID
	db, err := store.Open(context.Background(), filepath.Join(home, "platform", "generations", active.Generation, "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	marker := store.GitHubPATVerification{FingerprintSHA256: strings.Repeat("a", 64), Login: "marker", UserID: 9, Owner: "owner", Scopes: []string{"repo", "workflow"}, CredentialPath: filepath.Join(home, "state", "credentials", "github.pat"), Status: "marker", VerifiedAt: time.Now().UTC()}
	if err := db.RecordGitHubPATVerification(context.Background(), marker); err != nil {
		t.Fatal(err)
	}
	if count, err := db.BeginMaintenanceFence(context.Background(), store.MaintenanceFence{OperationID: "original-repair-fence", BundleDigest: active.BundleDigest}, time.Now().UTC()); err != nil || count != 0 {
		t.Fatalf("seed repair fence count=%d err=%v", count, err)
	}
	// Keep db open while repair runs: a backup/copy-over-self path would fail on
	// Windows or replace this durable marker.
	lifecycle.failReady = false
	request.ConsentID, request.AcceptedCapabilities, request.GitHubOwner = active.ConsentID, nil, ""
	if result, err := engine.Apply(context.Background(), request); err != nil || result.Status != "ready" {
		t.Fatalf("forward repair=%#v,%v", result, err)
	}
	after, err := ReadActive(home)
	if err != nil || after.Readiness != activeReady || after.AttemptID != originalAttempt || after.Generation != active.Generation {
		t.Fatalf("repaired active=%#v,%v", after, err)
	}
	if got, err := db.GitHubPATVerification(context.Background()); err != nil || got.Status != "marker" || got.Login != "marker" {
		t.Fatalf("business marker after repair=%#v,%v", got, err)
	}
	if count, err := db.BeginMaintenanceFence(context.Background(), store.MaintenanceFence{OperationID: "post-repair-fence", BundleDigest: active.BundleDigest}, time.Now().UTC()); err != nil || count != 0 {
		t.Fatalf("repair did not clear its target fence: count=%d err=%v", count, err)
	} else if err := db.ClearMaintenanceFence(context.Background(), "post-repair-fence"); err != nil {
		t.Fatal(err)
	}
	// A later live-readiness failure retains the same active repair identity.
	after.Readiness = activeRepairRequired
	if err := writeJSONAtomic(activePath(home), after); err != nil {
		t.Fatal(err)
	}
	lifecycle.failReady = true
	if result, err := engine.Apply(context.Background(), request); err != nil || result.Status != "repair_required" {
		t.Fatalf("failed forward repair=%#v,%v", result, err)
	}
	failed, err := ReadActive(home)
	if err != nil || failed.Readiness != activeRepairRequired || failed.AttemptID != originalAttempt || failed.Generation != active.Generation {
		t.Fatalf("failed repair active=%#v,%v", failed, err)
	}
}

func TestTargetInspectBindsFreshOwnerAndReusesOnlyUnchangedConsentOwner(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	digest := "sha256:" + strings.Repeat("e", 64)
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	engine := Engine{BundleRoot: bundle}
	fresh := Request{SchemaVersion: ProtocolVersion, Operation: Inspect, Purpose: PurposeTargetState, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner"}
	inspection, err := engine.Inspect(context.Background(), fresh)
	if err != nil || inspection.Status != "consent_required" {
		t.Fatalf("fresh inspect=%#v,%v", inspection, err)
	}
	capabilities, ok := inspection.Evidence["required_capabilities"].([]Capability)
	if !ok {
		t.Fatalf("capabilities=%#v", inspection.Evidence)
	}
	if owner, err := consentPATOwner(home, capabilities); err != nil || owner != "owner" {
		t.Fatalf("fresh PAT capability owner=%q, err=%v", owner, err)
	}

	apply := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: digest, GitHubOwner: "owner"}
	apply.AcceptedCapabilities = requiredCapabilities(t, engine, apply)
	if result, err := engine.Apply(context.Background(), apply); err != nil || result.Status != "ready" {
		t.Fatalf("apply=%#v,%v", result, err)
	}
	// Repair need not resend owner: the exact retained Consent supplies it.
	repair := fresh
	repair.GitHubOwner = ""
	inspection, err = engine.Inspect(context.Background(), repair)
	if err != nil || inspection.Status != "ready" {
		t.Fatalf("unchanged repair inspect=%#v,%v", inspection, err)
	}
	// A different explicit owner must produce a replacement-consent request.
	repair.GitHubOwner = "other-owner"
	inspection, err = engine.Inspect(context.Background(), repair)
	if err != nil || inspection.Status != "consent_required" {
		t.Fatalf("changed owner inspect=%#v,%v", inspection, err)
	}
	changed, ok := inspection.Evidence["required_capabilities"].([]Capability)
	if !ok {
		t.Fatalf("changed capabilities=%#v", inspection.Evidence)
	}
	if owner, err := consentPATOwner(home, changed); err != nil || owner != "other-owner" {
		t.Fatalf("changed PAT capability owner=%q, err=%v", owner, err)
	}
}

func TestLifecycleFailureKeepsAuthoritativeRepairRequired(t *testing.T) {
	bundle := t.TempDir()
	writeTestBundle(t, bundle, map[string]string{"platform/workflow.exe": "cli", "setup/workflow-setup.exe": "setup", "skills/agent-workflow/SKILL.md": "skill", "repository-contract/repository.json": "contract"})
	home := filepath.Join(t.TempDir(), "home")
	lifecycle := &fakeLifecycle{failReady: true}
	engine := Engine{BundleRoot: bundle, Lifecycle: lifecycle}
	request := Request{SchemaVersion: ProtocolVersion, Operation: Apply, WorkflowHome: home, TargetVersion: "0.0.1", BundleDigest: "sha256:" + strings.Repeat("d", 64), GitHubOwner: "owner"}
	request.AcceptedCapabilities = requiredCapabilities(t, engine, request)
	got, err := engine.Apply(context.Background(), request)
	if err != nil || got.Status != "repair_required" {
		t.Fatalf("apply=%#v,%v", got, err)
	}
	active, err := ReadActive(home)
	if err != nil || active.Readiness != activeRepairRequired {
		t.Fatalf("active=%#v,%v", active, err)
	}
}

func writeTestBundle(t *testing.T, root string, files map[string]string) {
	writeTestBundleVersion(t, root, "0.0.1", files)
}

func requiredCapabilities(t *testing.T, engine Engine, request Request) []Capability {
	t.Helper()
	capabilities, err := engine.requiredCapabilities(context.Background(), request, nil)
	if err != nil {
		t.Fatal(err)
	}
	return capabilities
}

func writeTestVerifiedReleaseManifest(t *testing.T, directory, version, bundleDigest string) *VerifiedReleaseManifest {
	t.Helper()
	manifest := workflowrelease.Manifest{
		SchemaVersion: 1, Version: version, CandidateSourceCommit: strings.Repeat("c", 40), QualificationRunID: 1, QualificationRunAttempt: 1,
		Bundle: workflowrelease.Bundle{Name: workflowrelease.BundleAssetName, SHA256: strings.TrimPrefix(bundleDigest, "sha256:")},
		Worker: workflowrelease.Worker{Image: "ghcr.io/skyhuang233/workflow-worker@sha256:" + strings.Repeat("a", 64), Tools: workflowrelease.Tools{
			Codex: workflowrelease.CodexTool{Version: "0.148.0"}, GitHubCLI: workflowrelease.ArchiveTool{Version: "2.97.0", LinuxAMD64SHA256: strings.Repeat("d", 64)},
			Go: workflowrelease.ArchiveTool{Version: "1.26.6", LinuxAMD64SHA256: strings.Repeat("e", 64)}, NoMistakes: workflowrelease.NoMistakesTool{Version: "v1.41.2", Repository: "skyhuang233/no-mistakes", Commit: strings.Repeat("f", 40)},
		}},
		SBOM: workflowrelease.SBOM{Name: workflowrelease.SBOMAssetName, Format: "spdx-json", SHA256: strings.Repeat("3", 64), Scan: workflowrelease.Scan{Scanner: "grype", SeverityCutoff: "high", OnlyFixed: true}},
	}
	raw, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, workflowrelease.ManifestAssetName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return &VerifiedReleaseManifest{ManifestPath: path, ManifestSHA256: hex.EncodeToString(digest[:]), SourceCommit: manifest.CandidateSourceCommit}
}

func writeTestBundleVersion(t *testing.T, root, version string, files map[string]string) {
	t.Helper()
	inventory := make([]workflowbundle.BundleFile, 0, len(files))
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		data := []byte(content)
		if err := os.WriteFile(full, data, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		inventory = append(inventory, workflowbundle.BundleFile{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
	}
	manifest := workflowbundle.BundleManifest{SchemaVersion: 1, SetupProtocolVersion: 1, Version: version, Compatibility: workflowbundle.Compatibility{OS: "windows", Architecture: "amd64", DatabaseSchema: store.LatestSchemaVersion, DockerDesktopVersion: "4.86.0", DockerInstallerURL: "https://example.test/docker.exe", DockerInstallerSHA256: strings.Repeat("b", 64), WorkerImage: "ghcr.io/skyhuang233/workflow-worker@sha256:" + strings.Repeat("a", 64)}, Files: inventory}
	raw, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "platform-release.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
