package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestGatewayCredentialVerificationStoresOnlyFingerprintAndContract(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	want := GatewayCredentialVerification{
		FingerprintSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Owner:             "owner", IntegrationRepository: "owner/integration",
		VerifiedAt: time.Date(2026, 7, 31, 2, 0, 0, 0, time.UTC),
	}
	if err := db.RecordGatewayCredentialVerification(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := db.GatewayCredentialVerification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("verification = %#v, want %#v", got, want)
	}
}
