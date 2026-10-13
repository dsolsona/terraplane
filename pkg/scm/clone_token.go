package scm

import (
	"errors"
	"time"
)

// ErrCloneTokenUnavailable is returned by providers that cannot mint
// repository-scoped tokens, such as one authenticated with a personal access
// token.
var ErrCloneTokenUnavailable = errors.New("clone tokens are not available")

// CloneToken is a short-lived credential that reads one repository's
// contents. It is handed to an agent for a single clone and revoked when the
// job ends.
type CloneToken struct {
	Token     string
	ExpiresAt time.Time
}
