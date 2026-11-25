// Package nodebackup seals a control plane's node state (its PKI, node.yaml
// and configuration) into one encrypted file, and puts such a file back on a
// machine that is to replace that control plane.
//
// An etcd snapshot holds only etcd's data. Without the cluster CA, the
// service-account key and the key that encrypts Secrets at rest, a replacement
// control plane mints a CA nothing trusts and cannot read the Secrets in the
// snapshot it restores. This package is the rest of the backup set.
//
// The file is encrypted to public keys. A node holds only recipients, which
// are not secret; the matching identities are generated off the cluster and
// kept by whoever would run a recovery. A copy of the file, or the storage it
// sits in, is therefore of no use to anyone without an identity.
package nodebackup

import (
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

const (
	recipientPrefix = "stratum-backup-recipient-"
	identityPrefix  = "stratum-backup-identity-"
	keyChecksumLen  = 4
)

// keyEncoding is the text form of a key: lower case and unpadded, so a key
// survives a YAML file, a shell and being read aloud.
var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func kem() hpke.KEM { return hpke.DHKEM(ecdh.X25519()) }

// Recipient is a public key a bundle is sealed to.
type Recipient struct {
	key hpke.PublicKey
	raw []byte
}

// Identity is the private key that opens a bundle sealed to its Recipient.
type Identity struct {
	key hpke.PrivateKey
}

// GenerateIdentity makes a new identity.
func GenerateIdentity() (Identity, error) {
	k, err := kem().GenerateKey()
	if err != nil {
		return Identity{}, fmt.Errorf("generate a backup identity: %w", err)
	}
	return Identity{key: k}, nil
}

// Recipient is the public half of id.
func (id Identity) Recipient() Recipient {
	pub := id.key.PublicKey()
	return Recipient{key: pub, raw: pub.Bytes()}
}

// String is the identity's text form. It is a secret: whoever holds it opens
// every bundle sealed to its recipient.
func (id Identity) String() (string, error) {
	b, err := id.key.Bytes()
	if err != nil {
		return "", fmt.Errorf("encode the backup identity: %w", err)
	}
	return encodeKey(identityPrefix, b), nil
}

// String is the recipient's text form, the value a cluster config carries.
func (r Recipient) String() string { return encodeKey(recipientPrefix, r.raw) }

// ParseRecipient reads a recipient's text form. A mistyped one is refused by
// its checksum, rather than sealing a bundle nobody can open.
func ParseRecipient(s string) (Recipient, error) {
	raw, err := decodeKey(recipientPrefix, s)
	if err != nil {
		return Recipient{}, fmt.Errorf("backup recipient: %w", err)
	}
	k, err := kem().NewPublicKey(raw)
	if err != nil {
		return Recipient{}, fmt.Errorf("backup recipient: %w", err)
	}
	return Recipient{key: k, raw: raw}, nil
}

// ParseRecipients reads every recipient of list, refusing a duplicate: two
// entries for one key look like two key holders and are one.
func ParseRecipients(list []string) ([]Recipient, error) {
	if len(list) > maxRecipients {
		return nil, fmt.Errorf("%d backup recipients, at most %d: a bundle is sealed to %d at most", len(list), maxRecipients, maxRecipients)
	}
	out := make([]Recipient, 0, len(list))
	seen := map[string]bool{}
	for _, s := range list {
		r, err := ParseRecipient(s)
		if err != nil {
			return nil, err
		}
		if seen[string(r.raw)] {
			return nil, fmt.Errorf("backup recipient %s is listed twice", r)
		}
		seen[string(r.raw)] = true
		out = append(out, r)
	}
	return out, nil
}

// ParseIdentity reads an identity's text form. The error never repeats the
// input, which is a secret.
func ParseIdentity(s string) (Identity, error) {
	raw, err := decodeKey(identityPrefix, strings.TrimSpace(s))
	if err != nil {
		return Identity{}, fmt.Errorf("backup identity: %w", err)
	}
	k, err := kem().NewPrivateKey(raw)
	if err != nil {
		return Identity{}, errors.New("backup identity: not a valid key")
	}
	return Identity{key: k}, nil
}

func keyChecksum(prefix string, key []byte) []byte {
	h := sha256.New()
	h.Write([]byte(prefix))
	h.Write(key)
	return h.Sum(nil)[:keyChecksumLen]
}

func encodeKey(prefix string, key []byte) string {
	body := append(append([]byte{}, key...), keyChecksum(prefix, key)...)
	return prefix + strings.ToLower(keyEncoding.EncodeToString(body))
}

func decodeKey(prefix, s string) ([]byte, error) {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return nil, fmt.Errorf("does not start with %q", prefix)
	}
	body, err := keyEncoding.DecodeString(strings.ToUpper(rest))
	if err != nil || len(body) <= keyChecksumLen {
		return nil, errors.New("is not a well-formed key")
	}
	key, sum := body[:len(body)-keyChecksumLen], body[len(body)-keyChecksumLen:]
	if subtle.ConstantTimeCompare(sum, keyChecksum(prefix, key)) != 1 {
		return nil, errors.New("fails its checksum: it was mistyped or cut short")
	}
	return key, nil
}
