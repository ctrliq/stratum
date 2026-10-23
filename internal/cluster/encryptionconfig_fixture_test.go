package cluster

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"
)

// testEncryptionKey is a base64-encoded 32-byte at-rest encryption key.
func testEncryptionKey(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// testEncryptionConfig is the EncryptionConfiguration a hardened profile mints
// as its shared secret: secretbox over secrets, with an identity fallback.
func testEncryptionConfig(keyB64 string) string {
	return fmt.Sprintf(`apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources: ["secrets"]
    providers:
      - secretbox:
          keys:
            - name: key1
              secret: %s
      - identity: {}
`, keyB64)
}
