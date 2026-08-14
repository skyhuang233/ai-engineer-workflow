package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	setupengine "github.com/skyhuang233/workflow/internal/setup"
	"github.com/skyhuang233/workflow/internal/store"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type setupResponse struct {
	Status             string `json:"status"`
	PlatformReady      bool   `json:"platform_ready,omitempty"`
	RepositoryAdmitted bool   `json:"repository_admitted,omitempty"`
	Blocker            string `json:"blocker,omitempty"`
	Result             any    `json:"result,omitempty"`
}

func setupCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("setup requires plan, apply, or verify")
	}
	switch args[0] {
	case "plan":
		return runSetupPlan(args[1:], os.Stdout)
	case "apply":
		return runSetupApply(args[1:], os.Stdin, os.Stdout)
	case "verify":
		return runSetupVerify(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown setup command %q", args[0])
	}
}

func runSetupPlan(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("setup plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repository := flags.String("repo", "", "absolute target repository")
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*repository) {
		return errors.New("setup plan requires an absolute --repo")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(layout.State, "workflow.db")); errors.Is(err, os.ErrNotExist) {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Bootstrap must be completed by the entry skill"})
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	if _, err := database.PlatformInstallation(context.Background()); err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Bootstrap must be completed by the entry skill"})
	}
	if _, err := database.GitHubPATVerification(context.Background()); err != nil {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Control Plane GitHub PAT verification is incomplete; rerun the approved Platform Bootstrap Plan"})
	}
	if admission, err := database.RepositoryAdmission(context.Background(), *repository); err == nil && admission.Eligible {
		return writeSetupResponse(output, setupResponse{Status: "ready", PlatformReady: true, RepositoryAdmitted: true})
	}
	return writeSetupResponse(output, setupResponse{Status: "blocked", PlatformReady: true, Blocker: "Repository Onboarding planner is not available in this build"})
}

func runSetupApply(args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("setup apply", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	planPath := flags.String("plan", "", "canonical Setup Plan path")
	approved := flags.String("approved-digest", "", "approved canonical SHA-256 digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *planPath == "" || *approved == "" {
		return errors.New("setup apply requires --plan and --approved-digest")
	}
	raw, err := os.ReadFile(*planPath)
	if err != nil {
		return err
	}
	var target struct {
		Target struct {
			WorkflowHome string `json:"workflow_home"`
		} `json:"target"`
	}
	if err := json.Unmarshal(raw, &target); err != nil {
		return err
	}
	layout, err := workflowhome.Resolve(target.Target.WorkflowHome)
	if err != nil {
		return err
	}
	engine := setupengine.Engine{Adapter: setupengine.HostAdapter{Layout: layout}, SecretInput: &setupengine.SecretInput{Reader: input}}
	result, applyErr := engine.Apply(context.Background(), raw, *approved)
	writeErr := writeSetupResponse(output, setupResponse{Status: string(result.Status), Result: result})
	return errors.Join(applyErr, writeErr)
}

func runSetupVerify(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("setup verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repository := flags.String("repo", "", "absolute target repository")
	homeOverride := flags.String("workflow-home", os.Getenv("WORKFLOW_HOME"), "absolute Workflow Home")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*repository) {
		return errors.New("setup verify requires an absolute --repo")
	}
	layout, err := workflowhome.Resolve(*homeOverride)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(layout.State, "workflow.db")); errors.Is(err, os.ErrNotExist) {
		return writeSetupResponse(output, setupResponse{Status: "blocked", Blocker: "Platform Installation state is unavailable"})
	}
	database, err := store.Open(context.Background(), filepath.Join(layout.State, "workflow.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	_, platformErr := database.PlatformInstallation(context.Background())
	pat, patErr := database.GitHubPATVerification(context.Background())
	platformReady := platformErr == nil && patErr == nil && pat.Status == "verified"
	admission, admissionErr := database.RepositoryAdmission(context.Background(), *repository)
	repositoryReady := admissionErr == nil && admission.Eligible
	status, blocker := "ready", ""
	if !platformReady || !repositoryReady {
		status, blocker = "blocked", "Platform Ready and Repository Admitted evidence are both required"
	}
	return writeSetupResponse(output, setupResponse{Status: status, PlatformReady: platformReady, RepositoryAdmitted: repositoryReady, Blocker: blocker})
}

func writeSetupResponse(output io.Writer, value setupResponse) error {
	return json.NewEncoder(output).Encode(value)
}
