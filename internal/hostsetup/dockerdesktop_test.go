package hostsetup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeDockerHost struct {
	version                  string
	download                 []byte
	downloadErr              error
	installCalls, startCalls int
	ready                    bool
}

func (f *fakeDockerHost) InstalledVersion(context.Context) (string, error) { return f.version, nil }
func (f *fakeDockerHost) Download(_ context.Context, _ string, path string) error {
	if err := os.WriteFile(path, f.download, 0o600); err != nil {
		return err
	}
	return f.downloadErr
}
func (f *fakeDockerHost) InstallElevated(context.Context, string) error {
	f.installCalls++
	f.version = "4.44.0"
	return nil
}
func (f *fakeDockerHost) Start(context.Context) error { f.startCalls++; f.ready = true; return nil }
func (f *fakeDockerHost) EngineReady(context.Context) error {
	if f.ready {
		return nil
	}
	return errors.New("not ready")
}

func TestEnsureDockerDesktopVerifiesAssetAndReadsBackEngine(t *testing.T) {
	asset := []byte("installer")
	sum := sha256.Sum256(asset)
	host := &fakeDockerHost{download: asset}
	contract := DockerDesktopContract{Version: "4.44.0", InstallerURL: "https://example.test/docker.exe", WindowsAMD64SHA256: hex.EncodeToString(sum[:])}
	if err := EnsureDockerDesktop(context.Background(), contract, host, t.TempDir(), time.Second); err != nil {
		t.Fatal(err)
	}
	if host.installCalls != 1 || host.startCalls != 1 {
		t.Fatalf("install=%d start=%d", host.installCalls, host.startCalls)
	}
	host.installCalls = 0
	if err := EnsureDockerDesktop(context.Background(), contract, host, t.TempDir(), time.Second); err != nil {
		t.Fatal(err)
	}
	if host.installCalls != 0 {
		t.Fatal("compatible Docker Desktop reinstalled")
	}
}
func TestEnsureDockerDesktopRejectsChecksumBeforeUAC(t *testing.T) {
	host := &fakeDockerHost{download: []byte("wrong")}
	contract := DockerDesktopContract{Version: "4.44.0", InstallerURL: "https://example.test/docker.exe", WindowsAMD64SHA256: repeatHex('a')}
	if err := EnsureDockerDesktop(context.Background(), contract, host, t.TempDir(), time.Second); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if host.installCalls != 0 {
		t.Fatal("unverified installer executed")
	}
}

func TestEnsureDockerDesktopCleansPartialDownloadBeforeRetry(t *testing.T) {
	asset := []byte("installer")
	sum := sha256.Sum256(asset)
	temporaryRoot := filepath.Join(t.TempDir(), "docker-downloads")
	host := &fakeDockerHost{download: []byte("partial"), downloadErr: errors.New("connection interrupted")}
	contract := DockerDesktopContract{Version: "4.44.0", InstallerURL: "https://example.test/docker.exe", WindowsAMD64SHA256: hex.EncodeToString(sum[:])}
	if err := EnsureDockerDesktop(context.Background(), contract, host, temporaryRoot, time.Second); err == nil {
		t.Fatal("partial download was accepted")
	}
	entries, err := os.ReadDir(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial download leaked retry-blocking files: %#v", entries)
	}
	host.download, host.downloadErr = asset, nil
	if err := EnsureDockerDesktop(context.Background(), contract, host, temporaryRoot, time.Second); err != nil {
		t.Fatalf("retry after partial download: %v", err)
	}
	entries, err = os.ReadDir(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("successful installer cleanup leaked files: %#v", entries)
	}
}
func repeatHex(value byte) string {
	result := make([]byte, 64)
	for i := range result {
		result[i] = value
	}
	return string(result)
}
