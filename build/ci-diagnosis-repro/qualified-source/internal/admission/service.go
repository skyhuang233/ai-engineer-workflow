// Package admission continuously fences new work by verified repository
// contract state. A failed repository never suspends unrelated repositories.
package admission

import (
	"context"
	"errors"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

var ErrRepositoryNotAdmitted = errors.New("repository is not admitted")

type Store interface {
	RepositoryAdmission(context.Context, string) (store.RepositoryAdmission, error)
	RecordRepositoryAdmission(context.Context, store.RepositoryAdmission) error
	RepositoryAdmissions(context.Context) ([]store.RepositoryAdmission, error)
}

type Verifier interface {
	Verify(context.Context, store.RepositoryAdmission) error
}

type Service struct {
	Store    Store
	Verifier Verifier
	Now      func() time.Time
}

func (s Service) Require(ctx context.Context, repository string) error {
	if s.Store == nil {
		return errors.New("Repository Admission store is unavailable")
	}
	value, err := s.Store.RepositoryAdmission(ctx, repository)
	if err != nil || !value.Eligible {
		return errors.Join(ErrRepositoryNotAdmitted, err)
	}
	return nil
}

func (s Service) VerifyAll(ctx context.Context) error {
	if s.Store == nil || s.Verifier == nil {
		return errors.New("Repository Admission verifier is incomplete")
	}
	values, err := s.Store.RepositoryAdmissions(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	var combined error
	for _, value := range values {
		verifyErr := s.Verifier.Verify(ctx, value)
		value.VerifiedAt = now
		value.Eligible = verifyErr == nil
		value.SuspensionReason = ""
		if verifyErr != nil {
			value.SuspensionReason = verifyErr.Error()
		}
		if recordErr := s.Store.RecordRepositoryAdmission(ctx, value); recordErr != nil {
			combined = errors.Join(combined, recordErr)
		}
	}
	return combined
}

func (s Service) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("Repository Admission verification interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.VerifyAll(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
