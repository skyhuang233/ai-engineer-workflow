package controlplane

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/startup"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

const (
	StateReady      = "ready"
	StateStopped    = "stopped"
	StateMismatched = "mismatched"
	StateStale      = "stale"
)

type Endpoints struct {
	Health   string `json:"health"`
	Shutdown string `json:"shutdown"`
}

type RuntimeRecord struct {
	PID                      int       `json:"pid"`
	PlatformVersion          string    `json:"platform_version"`
	ProcessStartedAt         time.Time `json:"process_started_at"`
	Endpoints                Endpoints `json:"endpoints"`
	ApprovedPlanDigestSHA256 string    `json:"approved_platform_bootstrap_plan_digest_sha256"`
}

func (r RuntimeRecord) Identity() Identity {
	return Identity{PID: r.PID, ProcessStartedAt: r.ProcessStartedAt, PlatformVersion: r.PlatformVersion, ApprovedPlanDigestSHA256: r.ApprovedPlanDigestSHA256}
}

func RuntimeRecordPath(layout workflowhome.Layout) string {
	return filepath.Join(layout.State, "runtime.json")
}

func LogPaths(layout workflowhome.Layout) (string, string) {
	return filepath.Join(layout.Logs, "control-plane.stdout.log"), filepath.Join(layout.Logs, "control-plane.stderr.log")
}

func WriteRuntimeRecord(layout workflowhome.Layout, record RuntimeRecord) error {
	if err := validateRuntimeRecord(record); err != nil {
		return err
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := RuntimeRecordPath(layout)
	temporary, err := os.CreateTemp(filepath.Dir(path), ".runtime-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceRuntimeFile(temporaryPath, path)
}

func ReadRuntimeRecord(layout workflowhome.Layout) (RuntimeRecord, error) {
	var record RuntimeRecord
	raw, err := os.ReadFile(RuntimeRecordPath(layout))
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return RuntimeRecord{}, fmt.Errorf("decode Control Plane Runtime Record: %w", err)
	}
	if err := validateRuntimeRecord(record); err != nil {
		return RuntimeRecord{}, err
	}
	return record, nil
}

func validateRuntimeRecord(record RuntimeRecord) error {
	if record.PID <= 0 || record.PlatformVersion == "" || record.ProcessStartedAt.IsZero() || !validDigest(record.ApprovedPlanDigestSHA256) {
		return errors.New("invalid Control Plane Runtime Record identity")
	}
	if !localHTTPURL(record.Endpoints.Health, "/health") || !localHTTPURL(record.Endpoints.Shutdown, "/shutdown") {
		return errors.New("Control Plane Runtime Record endpoints must be loopback HTTP endpoints")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func localHTTPURL(value, wantedPath string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Path != wantedPath || parsed.RawQuery != "" || parsed.User != nil {
		return false
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback() && parsed.Port() != ""
}

type ProcessIdentityFunc func(int) (time.Time, bool, error)

type Inspector struct {
	ProcessIdentity ProcessIdentityFunc
	Client          *http.Client
}

type Observation struct {
	State      string         `json:"state"`
	Record     *RuntimeRecord `json:"runtime,omitempty"`
	Diagnostic string         `json:"diagnostic,omitempty"`
}

type StartOptions struct {
	Layout                   workflowhome.Layout
	Executable               string
	PlatformVersion          string
	ApprovedPlanDigestSHA256 string
	Listen                   string
	Timeout                  time.Duration
	Inspector                Inspector
	Launch                   func(string, []string, string, string) (int, error)
}

func Start(ctx context.Context, options StartOptions) (RuntimeRecord, error) {
	if options.Executable == "" || options.PlatformVersion == "" || !validDigest(options.ApprovedPlanDigestSHA256) {
		return RuntimeRecord{}, errors.New("Control Plane launch requires executable, platform version, and approved bootstrap digest")
	}
	if err := options.Layout.Ensure(); err != nil {
		return RuntimeRecord{}, err
	}
	launchLock, err := startup.AcquireControlPlaneLaunchLock(options.Layout.Root)
	if err != nil {
		return RuntimeRecord{}, err
	}
	defer launchLock.Close()
	existing, readErr := ReadRuntimeRecord(options.Layout)
	if readErr == nil {
		observation := options.Inspector.Inspect(ctx, &existing)
		switch observation.State {
		case StateReady:
			if existing.PlatformVersion == options.PlatformVersion && existing.ApprovedPlanDigestSHA256 == options.ApprovedPlanDigestSHA256 {
				return existing, nil
			}
			return RuntimeRecord{}, errors.New("a different verified Control Plane instance is already running")
		case StateStale:
			// A stale observational record is replaced only after process absence
			// has been verified above.
		case StateStopped, StateMismatched:
			return RuntimeRecord{}, fmt.Errorf("refuse to replace live or mismatched Control Plane Runtime Record: %s", observation.Diagnostic)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return RuntimeRecord{}, fmt.Errorf("inspect existing Control Plane Runtime Record: %w", readErr)
	}
	listen := options.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	stdout, stderr := LogPaths(options.Layout)
	launch := options.Launch
	if launch == nil {
		launch = LaunchDetached
	}
	args := []string{"serve-child", "--workflow-home", options.Layout.Root, "--listen", listen, "--platform-version", options.PlatformVersion, "--approved-plan-digest", options.ApprovedPlanDigestSHA256}
	pid, err := launch(options.Executable, args, stdout, stderr)
	if err != nil {
		return RuntimeRecord{}, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	record, err := WaitReady(waitCtx, options.Layout, options.Inspector)
	if err != nil {
		return RuntimeRecord{}, err
	}
	if record.PID != pid {
		return RuntimeRecord{}, errors.New("healthy Control Plane PID does not match the launched foreground child")
	}
	return record, nil
}

func (i Inspector) Inspect(ctx context.Context, record *RuntimeRecord) Observation {
	if record == nil {
		return Observation{State: StateStopped, Diagnostic: "no Control Plane Runtime Record exists"}
	}
	processIdentity := i.ProcessIdentity
	if processIdentity == nil {
		processIdentity = OSProcessIdentity
	}
	started, live, err := processIdentity(record.PID)
	if err != nil {
		return Observation{State: StateMismatched, Record: record, Diagnostic: err.Error()}
	}
	if !live {
		return Observation{State: StateStale, Record: record, Diagnostic: "recorded process is not running"}
	}
	if !started.Equal(record.ProcessStartedAt) {
		return Observation{State: StateMismatched, Record: record, Diagnostic: "PID belongs to a different process start identity"}
	}
	client := i.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, record.Endpoints.Health, nil)
	if err != nil {
		return Observation{State: StateMismatched, Record: record, Diagnostic: err.Error()}
	}
	response, err := client.Do(request)
	if err != nil {
		return Observation{State: StateStopped, Record: record, Diagnostic: "recorded process is running but health is unavailable: " + err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Observation{State: StateStopped, Record: record, Diagnostic: fmt.Sprintf("health endpoint returned %d", response.StatusCode)}
	}
	var health Health
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&health); err != nil {
		return Observation{State: StateMismatched, Record: record, Diagnostic: "invalid health identity: " + err.Error()}
	}
	if health.Status != "ready" || health.Identity != record.Identity() {
		return Observation{State: StateMismatched, Record: record, Diagnostic: "health identity does not match the Runtime Record"}
	}
	return Observation{State: StateReady, Record: record}
}

func WaitReady(ctx context.Context, layout workflowhome.Layout, inspector Inspector) (RuntimeRecord, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var diagnostic string
	for {
		record, err := ReadRuntimeRecord(layout)
		if err == nil {
			observation := inspector.Inspect(ctx, &record)
			diagnostic = observation.Diagnostic
			if observation.State == StateReady {
				return record, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			diagnostic = err.Error()
		}
		select {
		case <-ctx.Done():
			return RuntimeRecord{}, fmt.Errorf("wait for Control Plane health (%s): %w", diagnostic, ctx.Err())
		case <-ticker.C:
		}
	}
}

func Stop(ctx context.Context, record RuntimeRecord, inspector Inspector) error {
	observation := inspector.Inspect(ctx, &record)
	if observation.State != StateReady {
		return fmt.Errorf("refuse to stop unverified Control Plane: %s: %s", observation.State, observation.Diagnostic)
	}
	client := inspector.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, record.Endpoints.Shutdown, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request graceful Control Plane shutdown: %w", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("graceful Control Plane shutdown returned %d", response.StatusCode)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	processIdentity := inspector.ProcessIdentity
	if processIdentity == nil {
		processIdentity = OSProcessIdentity
	}
	for {
		started, live, identityErr := processIdentity(record.PID)
		if identityErr == nil && (!live || !started.Equal(record.ProcessStartedAt)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for graceful Control Plane shutdown: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
