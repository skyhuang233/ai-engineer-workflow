package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/skyhuang233/workflow/internal/credential"
	"github.com/skyhuang233/workflow/internal/setupcontract"
	"github.com/skyhuang233/workflow/internal/workflowhome"
)

type HostAdapter struct {
	Layout     workflowhome.Layout
	Executable string
}

func (a HostAdapter) Readback(ctx context.Context, effect setupcontract.Effect) (setupcontract.EffectStatus, string, error) {
	switch effect.Kind {
	case "github_pat":
		_, err := credential.NewFileStore(a.Layout.CredentialFile).Get(ctx, credential.GatewayTarget)
		if errors.Is(err, credential.ErrNotFound) {
			return setupcontract.EffectRequired, "credential file is absent", nil
		}
		if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		return setupcontract.EffectSatisfied, "credential file exists", nil
	case "platform_cli":
		path := filepath.Join(a.Layout.Bin, workflowhome.ExecutableName)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return setupcontract.EffectRequired, "platform CLI is absent", nil
		} else if err != nil {
			return setupcontract.EffectFailed, "", err
		}
		return setupcontract.EffectSatisfied, "platform CLI exists", nil
	case "docker_desktop":
		command := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
		output, err := command.CombinedOutput()
		if err != nil {
			return setupcontract.EffectRequired, "Docker Desktop engine is unavailable", nil
		}
		if strings.TrimSpace(string(output)) != "linux/amd64" && strings.TrimSpace(string(output)) != "linux/x86_64" {
			return setupcontract.EffectConflicting, "Docker engine is not Linux amd64", nil
		}
		return setupcontract.EffectSatisfied, "Docker Linux amd64 engine is ready", nil
	default:
		return setupcontract.EffectConflicting, "unknown effect kind", nil
	}
}

func (a HostAdapter) Apply(ctx context.Context, effect setupcontract.Effect, input *SecretInput) error {
	switch effect.Kind {
	case "github_pat":
		secret, err := input.Read()
		if err != nil {
			return err
		}
		if err := workflowhome.SecureCredentialPath(a.Layout.CredentialFile, true); err != nil {
			return err
		}
		return credential.NewFileStore(a.Layout.CredentialFile).Set(ctx, credential.GatewayTarget, string(secret))
	case "platform_cli":
		source := a.Executable
		if source == "" {
			source, _ = os.Executable()
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		version := effect.Parameters["version"]
		if version == "" {
			return errors.New("platform CLI effect requires a version")
		}
		return (workflowhome.Installation{Layout: a.Layout}).InstallVersion(version, source, hex.EncodeToString(sum[:]))
	case "docker_desktop":
		return errors.New("Docker Desktop installer execution is not available in this build")
	default:
		return fmt.Errorf("unsupported Setup effect kind %q", effect.Kind)
	}
}
