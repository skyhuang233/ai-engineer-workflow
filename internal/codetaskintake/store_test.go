package codetaskintake

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/controlplane"
	"github.com/skyhuang233/workflow/internal/store"
)

func TestStoreIntakePersistsAndReusesOneTaskReference(t *testing.T) {
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	intake := StoreIntake{Store: database, Now: func() time.Time { return time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC) }}
	first, err := intake.AcceptIssue(context.Background(), "owner/repository", controlplane.ObservedIssue{ID: 3, Number: 1, Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := intake.AcceptIssue(context.Background(), "owner/repository", controlplane.ObservedIssue{ID: 3, Number: 1, Title: "changed"})
	if err != nil || first != second {
		t.Fatalf("first=%q second=%q err=%v", first, second, err)
	}
}
