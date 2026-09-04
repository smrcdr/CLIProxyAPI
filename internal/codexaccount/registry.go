package codexaccount

import (
	"strings"
	"sync"
)

var fingerprints sync.Map

// Register associates an internal auth ID with its opaque account fingerprint.
func Register(authID, fingerprint string) {
	authID = strings.ToLower(strings.TrimSpace(authID))
	fingerprint = strings.TrimSpace(fingerprint)
	if authID == "" || fingerprint == "" {
		return
	}
	fingerprints.Store(authID, fingerprint)
}

// FingerprintForAuthID returns the opaque fingerprint registered for an auth.
func FingerprintForAuthID(authID string) string {
	value, ok := fingerprints.Load(strings.ToLower(strings.TrimSpace(authID)))
	if !ok {
		return ""
	}
	fingerprint, _ := value.(string)
	return fingerprint
}

// Delete removes an auth ID association after hard deletion.
func Delete(authID string) {
	fingerprints.Delete(strings.ToLower(strings.TrimSpace(authID)))
}
