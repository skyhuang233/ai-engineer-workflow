package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSetupPlanReportsBootstrapBlockerWithoutMutatingRepository(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	repo := t.TempDir()
	var output bytes.Buffer
	if err := runSetupPlan([]string{"--repo", repo, "--workflow-home", home}, &output); err != nil {
		t.Fatal(err)
	}
	var response setupResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "blocked" || response.Blocker == "" {
		t.Fatalf("response=%#v", response)
	}
}

func TestSetupCommandsRequireAbsoluteRepository(t *testing.T) {
	for _, run := range []func([]string, *bytes.Buffer) error{func(args []string, out *bytes.Buffer) error { return runSetupPlan(args, out) }, func(args []string, out *bytes.Buffer) error { return runSetupVerify(args, out) }} {
		var output bytes.Buffer
		if err := run([]string{"--repo", "relative"}, &output); err == nil {
			t.Fatal("relative repository accepted")
		}
	}
}
