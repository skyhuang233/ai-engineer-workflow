package store

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
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
	rows, err := db.db.QueryContext(ctx, "PRAGMA table_info(gateway_credential_verifications)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range columns {
		lower := strings.ToLower(column)
		if strings.Contains(lower, "token") || strings.Contains(lower, "pem") || strings.Contains(lower, "private_key") || strings.Contains(lower, "jwt") {
			t.Fatalf("credential verification schema contains secret-bearing column %q", column)
		}
	}
	sort.Strings(columns)
	wantColumns := []string{"app_id", "fingerprint_sha256", "installation_id", "integration_repository", "owner", "singleton", "verified_at"}
	if strings.Join(columns, ",") != strings.Join(wantColumns, ",") {
		t.Fatalf("verification columns = %v, want %v", columns, wantColumns)
	}
	t.Logf("SQLite verification row persisted App ID %d, installation ID %d, PEM fingerprint, owner, repository, and verification time; schema columns contain no PEM, JWT, or installation-token field: %s",
		got.AppID, got.InstallationID, strings.Join(columns, ", "))
}
