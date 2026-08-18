// workflow-setup is both the versioned Setup Launcher carried in a Bundle and
// the tiny stable Workflow Dispatcher when installed as Workflow Home\bin\workflow.exe.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/skyhuang233/workflow/internal/launcher"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	if strings.EqualFold(filepath.Base(os.Args[0]), "workflow.exe") {
		return dispatch(args, input, output)
	}
	request, _, handled, err := decodeOrBlocked(input, output)
	if err != nil || handled {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	bundleRoot, err := launcherBundleRoot(executable)
	if err != nil {
		return err
	}
	lifecycle, err := launcher.NewBundleLifecycle(bundleRoot)
	if err != nil {
		return err
	}
	engine := launcher.Engine{BundleRoot: bundleRoot, ReconcilePath: workflowhome.PersistCurrentUserPath, Lifecycle: lifecycle, DependencyInspector: lifecycle}
	var result launcher.Result
	switch request.Operation {
	case launcher.Inspect:
		result, err = engine.Inspect(context.Background(), request)
	case launcher.Apply:
		result, err = engine.Apply(context.Background(), request)
	case launcher.Verify:
		result, err = engine.Verify(context.Background(), request)
	default:
		err = errors.New("unsupported setup operation")
	}
	if err != nil {
		return err
	}
	return encode(output, result)
}

// launcherBundleRoot accepts the two immutable layouts in which Launcher
// bytes execute: setup/workflow-setup.exe in a freshly extracted Bundle and
// workflow-setup.exe at the root of an active generation.  The latter must
// never depend on the temporary Bundle extraction surviving installation.
func launcherBundleRoot(executable string) (string, error) {
	directory := filepath.Dir(executable)
	for _, candidate := range []string{directory, filepath.Dir(directory)} {
		manifest := filepath.Join(candidate, "platform-release.json")
		info, err := os.Stat(manifest)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", errors.New("launcher cannot locate its immutable platform-release.json")
}

func dispatch(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("workflow dispatcher requires a command")
	}
	if args[0] == "setup" && len(args) > 1 && (args[1] == "inspect" || args[1] == "verify") {
		request, raw, handled, err := decodeOrBlocked(input, output)
		if err != nil || handled {
			return err
		}
		if (args[1] == "inspect" && request.Operation != launcher.Inspect) || (args[1] == "verify" && request.Operation != launcher.Verify) {
			return encode(output, launcher.Result{SchemaVersion: launcher.ProtocolVersion, Status: "blocked", Evidence: map[string]any{"error": "dispatcher command and protocol operation differ"}})
		}
		active, err := launcher.ReadActive(request.WorkflowHome)
		if err != nil {
			return encode(output, launcher.Result{SchemaVersion: launcher.ProtocolVersion, Status: "blocked", Evidence: map[string]any{"error": err.Error()}})
		}
		setup := filepath.Join(request.WorkflowHome, "platform", "generations", active.Generation, "workflow-setup.exe")
		return delegate(setup, args[1:], raw, output)
	}
	// Ordinary commands deliberately do no readiness repair or database work.
	// The versioned CLI receives the exact active generation only.
	home, err := workflowHomeArg(args)
	if err != nil {
		return err
	}
	active, err := launcher.ReadActive(home)
	if err != nil {
		return err
	}
	if active.Readiness != "ready" {
		return errors.New("workflow dispatcher requires a ready active generation")
	}
	database := filepath.Join(home, "platform", "generations", active.Generation, "workflow.db")
	return delegateWithEnvironment(filepath.Join(home, "platform", "generations", active.Generation, "workflow.exe"), args, nil, output, []string{
		"WORKFLOW_ACTIVE_HOME=" + home,
		"WORKFLOW_ACTIVE_GENERATION=" + active.Generation,
		"WORKFLOW_ACTIVE_DATABASE=" + database,
	})
}

func workflowHomeArg(args []string) (string, error) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--workflow-home" {
			return args[i+1], nil
		}
	}
	root := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if root == "" {
		return "", errors.New("--workflow-home is required when LOCALAPPDATA is unavailable")
	}
	return filepath.Join(root, "AgentWorkflow"), nil
}

func delegate(executable string, args []string, input []byte, output io.Writer) error {
	return delegateWithEnvironment(executable, args, input, output, nil)
}

func delegateWithEnvironment(executable string, args []string, input []byte, output io.Writer, environment []string) error {
	command := exec.Command(executable, args...)
	command.Stderr = os.Stderr
	command.Stdout = output
	if len(environment) > 0 {
		command.Env = append(os.Environ(), environment...)
	}
	if input != nil {
		stdin, err := command.StdinPipe()
		if err != nil {
			return err
		}
		go func() { _, _ = stdin.Write(input); _ = stdin.Close() }()
	}
	return command.Run()
}

func encode(output io.Writer, result launcher.Result) error {
	return json.NewEncoder(output).Encode(result)
}

func decodeOrBlocked(input io.Reader, output io.Writer) (launcher.Request, []byte, bool, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return launcher.Request{}, nil, false, err
	}
	request, err := launcher.DecodeRequest(raw)
	if err != nil {
		return launcher.Request{}, nil, true, encode(output, launcher.Result{SchemaVersion: launcher.ProtocolVersion, Status: "blocked", Evidence: map[string]any{"error": err.Error()}})
	}
	return request, raw, false, nil
}
