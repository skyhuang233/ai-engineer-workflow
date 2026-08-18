// Package launcher owns the small, versioned setup protocol and the durable
// generation based Workflow Home state. Host authority is represented by
// consent, never by an executable plan.
package launcher

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const ProtocolVersion = 1

type Operation string

const (
	Inspect Operation = "inspect"
	Apply   Operation = "apply"
	Verify  Operation = "verify"
)

const (
	PurposeActiveWorkPreflight = "active_work_preflight"
	PurposeTargetState         = "target_state"
)

type Capability struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PATCapabilityTarget is the unambiguous consent target for plaintext PAT
// persistence. Value is its canonical JSON representation, never a delimiter
// encoded owner/path pair.
type PATCapabilityTarget struct {
	Path  string `json:"path"`
	Owner string `json:"owner"`
}

type Request struct {
	SchemaVersion        int          `json:"schema_version"`
	Operation            Operation    `json:"operation"`
	WorkflowHome         string       `json:"workflow_home"`
	Purpose              string       `json:"purpose,omitempty"`
	TargetVersion        string       `json:"target_version,omitempty"`
	BundleDigest         string       `json:"bundle_digest,omitempty"`
	GitHubOwner          string       `json:"github_owner,omitempty"`
	AcceptedCapabilities []Capability `json:"accepted_capabilities,omitempty"`
	ConsentID            string       `json:"consent_id,omitempty"`
	PAT                  string       `json:"pat,omitempty"`
}

type Result struct {
	SchemaVersion int            `json:"schema_version"`
	Status        string         `json:"status"`
	Evidence      map[string]any `json:"evidence"`
}

// DecodeRequest is intentionally strict.  A newly installed skill must never
// make a newer launcher silently interpret a field as authority.
func DecodeRequest(raw []byte) (Request, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("decode setup request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Request{}, errors.New("setup request has trailing JSON")
		}
		return Request{}, fmt.Errorf("decode setup request: %w", err)
	}
	if request.SchemaVersion != ProtocolVersion {
		return Request{}, fmt.Errorf("unsupported setup protocol schema %d", request.SchemaVersion)
	}
	if strings.TrimSpace(request.WorkflowHome) == "" || !filepath.IsAbs(request.WorkflowHome) {
		return Request{}, errors.New("workflow_home must be an absolute path")
	}
	if request.Operation != Inspect && request.Operation != Apply && request.Operation != Verify {
		return Request{}, errors.New("unsupported setup operation")
	}
	if request.Operation == Inspect {
		if request.Purpose != PurposeActiveWorkPreflight && request.Purpose != PurposeTargetState {
			return Request{}, errors.New("inspect purpose is required")
		}
		if request.Purpose == PurposeTargetState {
			if err := validTarget(request); err != nil {
				return Request{}, err
			}
		}
	} else if request.Operation == Apply {
		if err := validTarget(request); err != nil {
			return Request{}, err
		}
		if (request.ConsentID == "") == (len(request.AcceptedCapabilities) == 0) {
			return Request{}, errors.New("apply requires exactly one consent_id or accepted_capabilities")
		}
	} else if request.Purpose != "" || request.TargetVersion != "" || request.BundleDigest != "" || request.GitHubOwner != "" || request.ConsentID != "" || len(request.AcceptedCapabilities) != 0 || request.PAT != "" {
		return Request{}, errors.New("verify accepts only common request fields")
	}
	return request, nil
}

func validTarget(request Request) error {
	digest := strings.TrimPrefix(request.BundleDigest, "sha256:")
	if strings.TrimSpace(request.TargetVersion) == "" || !strings.HasPrefix(request.BundleDigest, "sha256:") || len(digest) != 64 || digest != strings.ToLower(digest) {
		return errors.New("target_version and sha256 bundle_digest are required")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return errors.New("target_version and sha256 bundle_digest are required")
	}
	for _, c := range request.AcceptedCapabilities {
		if !validCapability(c) {
			return errors.New("accepted capability is invalid")
		}
	}
	return nil
}

func validCapability(c Capability) bool {
	return strings.TrimSpace(c.Name) != "" && strings.TrimSpace(c.Value) != ""
}
