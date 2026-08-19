package workflowrelease

import (
	"os"
	"strings"
	"testing"
)

func TestLoadToolchainSuppliesReleaseTools(t *testing.T) {
	config, err := LoadToolchain("../../config/toolchain.json")
	if err != nil {
		t.Fatal(err)
	}
	if config.Worker.ImageRepository != WorkerRepository {
		t.Fatalf("image repository = %q", config.Worker.ImageRepository)
	}
	if err := config.Tools().Validate(); err != nil {
		t.Fatalf("toolchain tools: %v", err)
	}
	if config.NoMistakes.Repository != "skyhuang233/no-mistakes" || config.NoMistakes.Commit != "eafc10e0fc7306be3af1750524aa2067e5048942" {
		t.Fatalf("no-mistakes pin = %#v", config.NoMistakes)
	}
}

func TestDecodeToolchainRejectsUnknownInputs(t *testing.T) {
	raw, err := os.ReadFile("../../config/toolchain.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), `"version": "0.148.0"`, `"version": "0.148.0", "untracked_option": true`, 1)
	if _, err := DecodeToolchain([]byte(changed)); err == nil {
		t.Fatal("DecodeToolchain accepted an unknown Codex option")
	}
}

func TestDecodeToolchainRejectsRetiredNoMistakesReleaseProvenance(t *testing.T) {
	raw, err := os.ReadFile("../../config/toolchain.json")
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), `"repository": "skyhuang233/no-mistakes"`, `"upstream_repository": "kunchenguid/no-mistakes", "repository": "skyhuang233/no-mistakes"`, 1)
	if _, err := DecodeToolchain([]byte(changed)); err == nil {
		t.Fatal("DecodeToolchain accepted retired no-mistakes Release provenance")
	}
}
