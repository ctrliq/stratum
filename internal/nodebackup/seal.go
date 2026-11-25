package nodebackup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hpke"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// The sealed form:
//
//	magic | version (1 byte) | recipient count (uint16) |
//	  per recipient: length (uint16) | the data key, HPKE-sealed to it |
//	nonce (12 bytes) | the body, AES-256-GCM under the data key
//
// Everything before the nonce is the body's additional data, so a header that
// was altered, or had a recipient added or removed, fails to open.
const (
	magic         = "STRATUM-NODE-STATE\n"
	formatVersion = 1
	dataKeyLen    = 32
	maxRecipients = 16
	maxWrappedLen = 1024
	// sealInfo binds a wrapped key to this use: one sealed for another
	// purpose under the same recipient does not open a bundle.
	sealInfo = "stratum node-state backup v1"
)

// ErrNotForIdentity is returned by Open when no recipient of the bundle is
// the identity's.
var ErrNotForIdentity = errors.New("this bundle was not sealed to that identity")

// Seal encrypts plaintext to every recipient. With none it refuses: there is
// no unencrypted form.
func Seal(recipients []Recipient, plaintext []byte) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("seal a node-state bundle: no recipient")
	}
	if len(recipients) > maxRecipients {
		return nil, fmt.Errorf("seal a node-state bundle: %d recipients, at most %d", len(recipients), maxRecipients)
	}
	dataKey := make([]byte, dataKeyLen)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(magic)
	out.WriteByte(formatVersion)
	_ = binary.Write(&out, binary.BigEndian, uint16(len(recipients)))
	for _, r := range recipients {
		wrapped, err := hpke.Seal(r.key, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(sealInfo), dataKey)
		if err != nil {
			return nil, fmt.Errorf("seal a node-state bundle to %s: %w", r, err)
		}
		_ = binary.Write(&out, binary.BigEndian, uint16(len(wrapped)))
		out.Write(wrapped)
	}
	gcm, err := bodyCipher(dataKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := bytes.Clone(out.Bytes())
	out.Write(nonce)
	out.Write(gcm.Seal(nil, nonce, plaintext, header))
	return out.Bytes(), nil
}

// Open decrypts a bundle with id. It fails on anything but a bundle sealed to
// id's recipient and unaltered since.
func Open(id Identity, sealed []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(sealed, []byte(magic))
	if !ok {
		return nil, errors.New("not a node-state bundle")
	}
	if len(rest) < 3 {
		return nil, errors.New("node-state bundle is cut short")
	}
	if rest[0] != formatVersion {
		return nil, fmt.Errorf("node-state bundle is format %d; this stratum reads format %d", rest[0], formatVersion)
	}
	n := int(binary.BigEndian.Uint16(rest[1:3]))
	rest = rest[3:]
	if n == 0 || n > maxRecipients {
		return nil, errors.New("node-state bundle names an impossible number of recipients")
	}
	var dataKey []byte
	for range n {
		if len(rest) < 2 {
			return nil, errors.New("node-state bundle is cut short")
		}
		l := int(binary.BigEndian.Uint16(rest[:2]))
		rest = rest[2:]
		if l > maxWrappedLen || len(rest) < l {
			return nil, errors.New("node-state bundle is cut short")
		}
		wrapped := rest[:l]
		rest = rest[l:]
		if dataKey != nil {
			continue
		}
		if k, err := hpke.Open(id.key, hpke.HKDFSHA256(), hpke.AES256GCM(), []byte(sealInfo), wrapped); err == nil && len(k) == dataKeyLen {
			dataKey = k
		}
	}
	if dataKey == nil {
		return nil, ErrNotForIdentity
	}
	header := sealed[:len(sealed)-len(rest)]
	gcm, err := bodyCipher(dataKey)
	if err != nil {
		return nil, err
	}
	if len(rest) < gcm.NonceSize() {
		return nil, errors.New("node-state bundle is cut short")
	}
	plaintext, err := gcm.Open(nil, rest[:gcm.NonceSize()], rest[gcm.NonceSize():], header)
	if err != nil {
		return nil, errors.New("node-state bundle is damaged or was altered: it does not authenticate")
	}
	return plaintext, nil
}

func bodyCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
