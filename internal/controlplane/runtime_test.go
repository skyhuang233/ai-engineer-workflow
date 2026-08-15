package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func TestRuntimeRecordRoundTripAndLogPathsStayInWorkflowHome(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := RuntimeRecord{PID: 123, PlatformVersion: "1.2.3", ProcessStartedAt: time.Now().UTC().Round(0), Endpoints: Endpoints{Health: "http://127.0.0.1:12345/health", Shutdown: "http://127.0.0.1:12345/shutdown"}, ApprovedPlanDigestSHA256: repeatDigest('c')}
	if err := WriteRuntimeRecord(layout, record); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRuntimeRecord(layout)
	if err != nil {
		t.Fatal(err)
	}
	if got != record {
		t.Fatalf("record = %#v, want %#v", got, record)
	}
	stdout, stderr := LogPaths(layout)
	for _, path := range []string{stdout, stderr} {
		if filepath.Dir(path) != layout.Logs {
			t.Fatalf("log escaped Workflow Home: %q", path)
		}
	}
	data, err := os.ReadFile(RuntimeRecordPath(layout))
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid(data) == false {
		t.Fatal("runtime record is not JSON")
	}
}

func TestInspectorDistinguishesReadyStaleMismatchedAndStopped(t *testing.T) {
	started := time.Now().UTC().Round(0)
	record := RuntimeRecord{PID: 123, PlatformVersion: "1.2.3", ProcessStartedAt: started, ApprovedPlanDigestSHA256: repeatDigest('d')}
	healthIdentity := record.Identity()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Health{Status: "ready", Identity: healthIdentity})
	}))
	defer server.Close()
	record.Endpoints = Endpoints{Health: server.URL}

	inspector := Inspector{ProcessIdentity: func(pid int) (time.Time, bool, error) { return started, pid == 123, nil }, Client: server.Client()}
	if observation := inspector.Inspect(context.Background(), &record); observation.State != StateReady {
		t.Fatalf("ready observation = %#v", observation)
	}
	inspector.ProcessIdentity = func(int) (time.Time, bool, error) { return time.Time{}, false, nil }
	if observation := inspector.Inspect(context.Background(), &record); observation.State != StateStale {
		t.Fatalf("stale observation = %#v", observation)
	}
	inspector.ProcessIdentity = func(int) (time.Time, bool, error) { return started.Add(time.Second), true, nil }
	if observation := inspector.Inspect(context.Background(), &record); observation.State != StateMismatched {
		t.Fatalf("reused PID observation = %#v", observation)
	}
	if observation := inspector.Inspect(context.Background(), nil); observation.State != StateStopped {
		t.Fatalf("stopped observation = %#v", observation)
	}

	inspector.ProcessIdentity = func(int) (time.Time, bool, error) { return started, true, nil }
	healthIdentity.PlatformVersion = "other"
	if observation := inspector.Inspect(context.Background(), &record); observation.State != StateMismatched {
		t.Fatalf("health mismatch observation = %#v", observation)
	}
}

func TestInspectorReportsStoppedWhenOwnedProcessIsUnhealthy(t *testing.T) {
	started := time.Now().UTC().Round(0)
	record := RuntimeRecord{PID: 123, PlatformVersion: "1.2.3", ProcessStartedAt: started, Endpoints: Endpoints{Health: "http://127.0.0.1:1/health"}, ApprovedPlanDigestSHA256: repeatDigest('e')}
	observation := (Inspector{ProcessIdentity: func(int) (time.Time, bool, error) { return started, true, nil }, Client: &http.Client{Timeout: 50 * time.Millisecond}}).Inspect(context.Background(), &record)
	if observation.State != StateStopped {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestStartLaunchesOnceAndReturnsExistingMatchingInstance(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Round(0)
	digest := repeatDigest('f')
	launches := 0
	var server *httptest.Server
	inspector := Inspector{ProcessIdentity: func(pid int) (time.Time, bool, error) { return started, pid == 321, nil }}
	launch := func(_ string, args []string, stdout, stderr string) (int, error) {
		launches++
		if filepath.Dir(stdout) != layout.Logs || filepath.Dir(stderr) != layout.Logs || len(args) == 0 || args[0] != "serve-child" {
			t.Fatalf("launch = args %#v stdout %q stderr %q", args, stdout, stderr)
		}
		record := RuntimeRecord{PID: 321, PlatformVersion: "1.0.0", ProcessStartedAt: started, ApprovedPlanDigestSHA256: digest}
		server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(Health{Status: "ready", Identity: record.Identity()})
		}))
		record.Endpoints = Endpoints{Health: server.URL + "/health", Shutdown: server.URL + "/shutdown"}
		if err := WriteRuntimeRecord(layout, record); err != nil {
			return 0, err
		}
		return 321, nil
	}
	options := StartOptions{Layout: layout, Executable: `C:\workflow.exe`, PlatformVersion: "1.0.0", ApprovedPlanDigestSHA256: digest, Timeout: time.Second, Inspector: inspector, Launch: launch}
	if _, err := Start(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if _, err := Start(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if launches != 1 {
		t.Fatalf("launches = %d", launches)
	}
}

func TestStartReplacesDifferentVerifiedCurrentUserInstanceOnlyWhenAuthorized(t *testing.T) {
	layout, err := workflowhome.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	oldStarted, newStarted := time.Now().UTC().Add(-time.Minute).Round(0), time.Now().UTC().Round(0)
	oldDigest, newDigest := repeatDigest('a'), repeatDigest('b')
	oldLive, launches, authorized := true, 0, 0
	old := RuntimeRecord{PID: 111, PlatformVersion: "1.0.0", ProcessStartedAt: oldStarted, ApprovedPlanDigestSHA256: oldDigest}
	var launched RuntimeRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			oldLive = false
			_, _ = w.Write([]byte("ok"))
			return
		}
		identity := old.Identity()
		if !oldLive {
			identity = launched.Identity()
		}
		_ = json.NewEncoder(w).Encode(Health{Status: "ready", Identity: identity})
	}))
	defer server.Close()
	old.Endpoints = Endpoints{Health: server.URL + "/health", Shutdown: server.URL + "/shutdown"}
	if err := WriteRuntimeRecord(layout, old); err != nil {
		t.Fatal(err)
	}
	inspector := Inspector{Client: server.Client(), ProcessIdentity: func(pid int) (time.Time, bool, error) {
		if pid == old.PID {
			return oldStarted, oldLive, nil
		}
		return newStarted, pid == 222, nil
	}}
	launch := func(_ string, _ []string, _, _ string) (int, error) {
		launches++
		launched = RuntimeRecord{PID: 222, PlatformVersion: "2.0.0", ProcessStartedAt: newStarted, Endpoints: old.Endpoints, ApprovedPlanDigestSHA256: newDigest}
		return 222, WriteRuntimeRecord(layout, launched)
	}
	options := StartOptions{Layout: layout, Executable: `C:\workflow.exe`, PlatformVersion: "2.0.0", ApprovedPlanDigestSHA256: newDigest, Timeout: time.Second, Inspector: inspector, Launch: launch, Replace: true, AuthorizeReplacement: func(pid int) error {
		authorized++
		if pid != 111 {
			t.Fatalf("authorized pid %d", pid)
		}
		return nil
	}}
	denied := options
	denied.AuthorizeReplacement = func(int) error { return errors.New("different user") }
	if _, err := Start(context.Background(), denied); err == nil || !oldLive || launches != 0 {
		t.Fatalf("unsafe replacement was not rejected: err=%v oldLive=%v launches=%d", err, oldLive, launches)
	}
	if _, err := Start(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if authorized != 1 || launches != 1 || oldLive {
		t.Fatalf("authorized=%d launches=%d oldLive=%v", authorized, launches, oldLive)
	}
}
