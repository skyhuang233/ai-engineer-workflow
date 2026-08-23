package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/skyhuang233/workflow/internal/workflowbundle"
)

const (
	dockerActionReuse   = "reuse"
	dockerActionInstall = "install"
	dockerActionUpgrade = "upgrade"
)

// DockerCapabilityTarget is the exact host change to which the user consents.
// It is canonical JSON inside manage_docker_desktop, never a generic label.
type DockerCapabilityTarget struct {
	Action          string `json:"action"`
	RequiredVersion string `json:"required_version"`
	ObservedVersion string `json:"observed_version,omitempty"`
	InstallerURL    string `json:"installer_url,omitempty"`
	InstallerSHA256 string `json:"installer_sha256,omitempty"`
	HostImpact      string `json:"host_impact"`
}

// WorkerImageCapabilityTarget binds the exact immutable image digest to the
// pull_worker_image capability.
type WorkerImageCapabilityTarget struct {
	Image string `json:"image"`
}

func (e Engine) bundleCompatibility() (workflowbundle.Compatibility, error) {
	if strings.TrimSpace(e.BundleRoot) == "" {
		return workflowbundle.Compatibility{}, errors.New("launcher bundle root is required")
	}
	raw, err := os.ReadFile(filepath.Join(e.BundleRoot, "platform-release.json"))
	if err != nil {
		return workflowbundle.Compatibility{}, fmt.Errorf("read Bundle manifest: %w", err)
	}
	var manifest workflowbundle.BundleManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return workflowbundle.Compatibility{}, fmt.Errorf("decode Bundle manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return workflowbundle.Compatibility{}, err
	}
	return manifest.Compatibility, nil
}

func (e Engine) dockerConsentTarget(ctx context.Context, compatibility workflowbundle.Compatibility) (DockerCapabilityTarget, error) {
	observed := ""
	if e.DependencyInspector != nil {
		version, err := e.DependencyInspector.DockerVersion(ctx)
		if err != nil {
			return DockerCapabilityTarget{}, fmt.Errorf("inspect Docker Desktop: %w", err)
		}
		observed = strings.TrimSpace(version)
	}
	if observed == compatibility.DockerDesktopVersion {
		return DockerCapabilityTarget{Action: dockerActionReuse, RequiredVersion: compatibility.DockerDesktopVersion, ObservedVersion: observed, HostImpact: "reuse verified Docker Desktop"}, nil
	}
	action := dockerActionInstall
	impact := "install Docker Desktop for current Windows user"
	if observed != "" {
		action = dockerActionUpgrade
		impact = "upgrade Docker Desktop for current Windows user"
	}
	return DockerCapabilityTarget{Action: action, RequiredVersion: compatibility.DockerDesktopVersion, ObservedVersion: observed, InstallerURL: compatibility.DockerInstallerURL, InstallerSHA256: compatibility.DockerInstallerSHA256, HostImpact: impact}, nil
}

func (target DockerCapabilityTarget) valid() error {
	if target.Action != dockerActionReuse && target.Action != dockerActionInstall && target.Action != dockerActionUpgrade {
		return errors.New("Docker consent action is invalid")
	}
	if strings.TrimSpace(target.RequiredVersion) == "" || strings.TrimSpace(target.HostImpact) == "" {
		return errors.New("Docker consent target is incomplete")
	}
	if target.Action == dockerActionReuse {
		if target.ObservedVersion != target.RequiredVersion || target.InstallerURL != "" || target.InstallerSHA256 != "" {
			return errors.New("Docker reuse target is invalid")
		}
		return nil
	}
	if target.InstallerURL == "" || target.InstallerSHA256 == "" {
		return errors.New("Docker install target is incomplete")
	}
	return nil
}

func validateDockerCapability(target DockerCapabilityTarget, compatibility workflowbundle.Compatibility) error {
	if err := target.valid(); err != nil {
		return err
	}
	if target.RequiredVersion != compatibility.DockerDesktopVersion {
		return errors.New("Docker consent version differs from Bundle manifest")
	}
	if target.Action != dockerActionReuse && (target.InstallerURL != compatibility.DockerInstallerURL || target.InstallerSHA256 != compatibility.DockerInstallerSHA256) {
		return errors.New("Docker consent installer differs from Bundle manifest")
	}
	return nil
}

func (target WorkerImageCapabilityTarget) valid() error {
	marker := strings.LastIndex(target.Image, "@sha256:")
	if marker <= 0 || len(target.Image[marker+len("@sha256:"):]) != 64 {
		return errors.New("worker image consent must be immutable image@sha256")
	}
	return nil
}

func dockerCapability(values []Capability) (DockerCapabilityTarget, error) {
	for _, value := range values {
		if value.Name == "manage_docker_desktop" {
			var target DockerCapabilityTarget
			if err := json.Unmarshal([]byte(value.Value), &target); err != nil {
				return DockerCapabilityTarget{}, errors.New("manage_docker_desktop requires canonical structured target")
			}
			if err := target.valid(); err != nil {
				return DockerCapabilityTarget{}, err
			}
			return target, nil
		}
	}
	return DockerCapabilityTarget{}, errors.New("manage_docker_desktop capability is required")
}

func workerImageCapability(values []Capability) (WorkerImageCapabilityTarget, error) {
	for _, value := range values {
		if value.Name == "pull_worker_image" {
			var target WorkerImageCapabilityTarget
			if err := json.Unmarshal([]byte(value.Value), &target); err != nil {
				return WorkerImageCapabilityTarget{}, errors.New("pull_worker_image requires canonical structured target")
			}
			if err := target.valid(); err != nil {
				return WorkerImageCapabilityTarget{}, err
			}
			return target, nil
		}
	}
	return WorkerImageCapabilityTarget{}, errors.New("pull_worker_image capability is required")
}
