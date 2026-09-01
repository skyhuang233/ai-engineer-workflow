package setup

import (
	"context"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/store"
)

type fakeService struct{ present, registered bool }

func (s *fakeService) Present(context.Context) (bool, error) { return s.present, nil }
func (s *fakeService) Register(context.Context) error        { s.registered = true; return nil }

type activationWatches struct{ watch store.RepositoryWatch }

func (w activationWatches) RepositoryWatch(context.Context, string) (store.RepositoryWatch, error) {
	return w.watch, nil
}

func TestActivateReusesPriorCheckpointWhenNothingChanged(t *testing.T) {
	registered := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	service := &fakeService{present: true}
	result, err := Activate(context.Background(), service, activationWatches{watch: store.RepositoryWatch{Repository: "owner/repository", RegisteredAt: registered, LastSuccessfulPollAt: registered.Add(time.Second)}}, store.RepositoryWatch{Repository: "owner/repository", RegisteredAt: registered, LastSuccessfulPollAt: registered.Add(time.Second)}, false, time.Second, func() time.Time { return registered })
	if err != nil || result.ServiceRegistered {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestActivateRequiresPostRegistrationPoll(t *testing.T) {
	registered := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	service := &fakeService{}
	result, err := Activate(context.Background(), service, activationWatches{watch: store.RepositoryWatch{Repository: "owner/repository", RegisteredAt: registered, LastSuccessfulPollAt: registered.Add(time.Second)}}, store.RepositoryWatch{Repository: "owner/repository", RegisteredAt: registered}, true, 10*time.Millisecond, func() time.Time { return registered.Add(2 * time.Second) })
	if err == nil || !service.registered || !result.ServiceRegistered {
		t.Fatalf("result=%+v err=%v service=%+v", result, err, service)
	}
}
