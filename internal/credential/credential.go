package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const GatewayTarget = "workflow/github-gateway"

var ErrNotFound = errors.New("credential not found")

type Store interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string) error
}

func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}
