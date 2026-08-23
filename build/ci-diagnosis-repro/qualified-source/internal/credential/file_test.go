package credential

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreRoundTripAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "credentials", "github.pat")
	store := NewFileStore(path)
	for _, token := range []string{"ghp_first", "ghp_second"} {
		if err := store.Set(context.Background(), GatewayTarget, "  "+token+"\n"); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(context.Background(), GatewayTarget)
		if err != nil || got != token {
			t.Fatalf("Get = %q, %v", got, err)
		}
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "ghp_second\n" {
		t.Fatalf("file = %q, %v", data, err)
	}
}

func TestFileStoreDoesNotExposeTokenInErrors(t *testing.T) {
	token := "ghp_do-not-leak"
	store := NewFileStore(filepath.Join(t.TempDir(), "missing", "github.pat"))
	_, err := store.Get(context.Background(), GatewayTarget)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v", err)
	}
	if err != nil && err.Error() == token {
		t.Fatal("token leaked")
	}
}
