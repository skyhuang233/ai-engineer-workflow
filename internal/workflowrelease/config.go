package workflowrelease

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	hex64Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Config struct {
	SchemaVersion int           `json:"schema_version"`
	Version       string        `json:"version"`
	DockerDesktop DockerDesktop `json:"docker_desktop"`
}

type DockerDesktop struct {
	Version            string `json:"version"`
	InstallerURL       string `json:"installer_url"`
	WindowsAMD64SHA256 string `json:"windows_amd64_sha256"`
}

func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read Workflow Release configuration: %w", err)
	}
	return DecodeConfig(raw)
}

func DecodeConfig(raw []byte) (Config, error) {
	var config Config
	if err := decodeStrict(raw, &config); err != nil {
		return Config{}, fmt.Errorf("decode Workflow Release configuration: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	switch {
	case c.SchemaVersion != 1:
		return errors.New("unsupported Workflow Release configuration schema")
	case validateSemver(c.Version) != nil:
		return fmt.Errorf("version: %w", validateSemver(c.Version))
	case strings.TrimSpace(c.DockerDesktop.Version) == "":
		return errors.New("Docker Desktop version is required")
	case !validHTTPSURL(c.DockerDesktop.InstallerURL):
		return errors.New("Docker Desktop installer URL must be absolute HTTPS")
	case !hex64Pattern.MatchString(c.DockerDesktop.WindowsAMD64SHA256):
		return errors.New("Docker Desktop Windows amd64 installer SHA-256 must be lowercase hexadecimal")
	}
	return nil
}

func validateSemver(value string) error {
	parts := semverPattern.FindStringSubmatch(value)
	if parts == nil {
		return errors.New("must be a bare semantic version core")
	}
	for _, part := range parts[1:] {
		value, err := strconv.ParseUint(part, 10, 31)
		if err != nil || value > 2147483647 {
			return errors.New("components must fit signed 32-bit range")
		}
	}
	return nil
}

func validHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs() && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}
