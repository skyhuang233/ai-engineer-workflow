package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkflowVersionIsExplicitForDevelopmentAndReleaseBuilds(t *testing.T) {
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	buildAndReadVersion := func(t *testing.T, ldflags ...string) string {
		t.Helper()
		executable := filepath.Join(t.TempDir(), "workflow"+extension)
		arguments := []string{"build", "-o", executable}
		if len(ldflags) > 0 {
			arguments = append(arguments, "-ldflags", strings.Join(ldflags, " "))
		}
		arguments = append(arguments, ".")
		command := exec.Command("go", arguments...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build workflow: %v\n%s", err, output)
		}
		output, err := exec.Command(executable, "version").CombinedOutput()
		if err != nil {
			t.Fatalf("workflow version: %v\n%s", err, output)
		}
		return strings.TrimSpace(string(output))
	}

	if got := buildAndReadVersion(t); got != "workflow dev" {
		t.Fatalf("development version = %q, want %q", got, "workflow dev")
	}
	if got := buildAndReadVersion(t, "-X", "main.Version=1.2.3"); got != "workflow 1.2.3" {
		t.Fatalf("published version = %q, want %q", got, "workflow 1.2.3")
	}
}

func TestWorkflowVersionCommandRejectsNonCanonicalPublishedVersion(t *testing.T) {
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	for _, version := range []string{"v1.2.3", "01.2.3", "1.2.3-rc.1", "1.2.3+build.1"} {
		t.Run(version, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "workflow"+extension)
			command := exec.Command("go", "build", "-o", executable, "-ldflags", "-X main.Version="+version, ".")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("build workflow: %v\n%s", err, output)
			}
			output, err := exec.Command(executable, "version").CombinedOutput()
			if err == nil || !strings.Contains(string(output), "bare semantic version core") {
				t.Fatalf("workflow accepted published version %q: err=%v output=%s", version, err, output)
			}
			output, err = exec.Command(executable, "not-a-command").CombinedOutput()
			if err == nil || !strings.Contains(string(output), "bare semantic version core") {
				t.Fatalf("workflow command startup bypassed invalid published version %q: err=%v output=%s", version, err, output)
			}
		})
	}
}
