package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestGitHubAppVerificationStoresOnlyFingerprintAndInstallationIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	want := GitHubAppVerification{
		FingerprintSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AppID:             123, InstallationID: 456,
		Owner: "owner", IntegrationRepository: "owner/integration",
		VerifiedAt: time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC),
	}
	if err := db.RecordGitHubAppVerification(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := db.GitHubAppVerification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("verification = %#v, want %#v", got, want)
	}
}
