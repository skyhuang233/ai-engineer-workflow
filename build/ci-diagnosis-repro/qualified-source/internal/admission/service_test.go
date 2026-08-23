package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type memoryStore struct {
	values map[string]store.RepositoryAdmission
}

func (m *memoryStore) RepositoryAdmission(_ context.Context, repository string) (store.RepositoryAdmission, error) {
	value, ok := m.values[repository]
	if !ok {
		return store.RepositoryAdmission{}, store.ErrNotFound
	}
	return value, nil
}
func (m *memoryStore) RecordRepositoryAdmission(_ context.Context, value store.RepositoryAdmission) error {
	m.values[value.Repository] = value
	return nil
}
func (m *memoryStore) RepositoryAdmissions(context.Context) ([]store.RepositoryAdmission, error) {
	result := []store.RepositoryAdmission{}
	for _, v := range m.values {
		result = append(result, v)
	}
	return result, nil
}

type verifierFunc func(context.Context, store.RepositoryAdmission) error

func (f verifierFunc) Verify(ctx context.Context, value store.RepositoryAdmission) error {
	return f(ctx, value)
}

func TestVerifyAllSuspendsOnlyDriftedRepository(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	values := map[string]store.RepositoryAdmission{
		"owner/good": {Repository: "owner/good", Eligible: true, VerifiedAt: now},
		"owner/bad":  {Repository: "owner/bad", Eligible: true, VerifiedAt: now},
	}
	service := Service{Store: &memoryStore{values: values}, Now: func() time.Time { return now.Add(time.Hour) }, Verifier: verifierFunc(func(_ context.Context, value store.RepositoryAdmission) error {
		if value.Repository == "owner/bad" {
			return errors.New("manifest drift")
		}
		return nil
	})}
	if err := service.VerifyAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !values["owner/good"].Eligible || values["owner/bad"].Eligible || values["owner/bad"].SuspensionReason != "manifest drift" {
		t.Fatalf("values=%#v", values)
	}
}
