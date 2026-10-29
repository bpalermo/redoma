// Package modelauth reads how the model listeners present the model API
// credential: <etc>/model-auth, one word. The host's administrator writes
// it; this package only reads it.
//
// The credential itself is the SDS secret anthropic-key, which holds the
// complete header value. The mode only says which header Envoy writes it to.
package modelauth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Mode is how a model listener injects the credential.
type Mode int

const (
	// None injects no credential and strips both headers: a request reaches
	// the API with none and gets a 401. It is what an unreadable or unknown
	// mode means, so an error never sends a credential the wrong way.
	None Mode = iota
	// APIKey writes the secret to x-api-key and strips authorization.
	APIKey
	// Bearer writes the secret to authorization, as the header's whole
	// value, replacing the value the client sent, and strips x-api-key.
	Bearer
)

// String is the mode's word in the file; None's is "none".
func (m Mode) String() string {
	switch m {
	case APIKey:
		return "api-key"
	case Bearer:
		return "bearer"
	default:
		return "none"
	}
}

// Path is the mode file under etcDir.
func Path(etcDir string) string {
	return filepath.Join(etcDir, "model-auth")
}

// Read returns the mode in etcDir. A missing file is APIKey, the mode from
// before the file existed. A file that cannot be read, or whose word (with
// surrounding whitespace trimmed) is not a known mode, is None and an
// error. The error never quotes the file: a credential written there by
// mistake must not reach a log.
func Read(etcDir string) (Mode, error) {
	path := Path(etcDir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return APIKey, nil
	}
	if err != nil {
		return None, err
	}
	switch strings.TrimSpace(string(raw)) {
	case "api-key":
		return APIKey, nil
	case "bearer":
		return Bearer, nil
	default:
		return None, fmt.Errorf("%s: not a known mode, such as \"api-key\" (%d bytes)", path, len(raw))
	}
}
