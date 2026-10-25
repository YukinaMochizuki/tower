package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// LoadPublicKey reads tower's public key, as the apiserver writes it
// beside its signing key: one line of standard base64.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%s: a ticket public key is %d bytes in standard base64, on one line", path, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(key), nil
}
