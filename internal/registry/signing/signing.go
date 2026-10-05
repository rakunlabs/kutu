// Package signing holds the repository-metadata signing helpers shared
// by the apt, rpm and alpine registries: OpenPGP clear/detached
// signatures from an ASCII-armored private key and RSA keys for
// alpine APKINDEX signatures.
package signing

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// PGPKey is a parsed OpenPGP signing key.
type PGPKey struct {
	entity *openpgp.Entity
}

func pgpConfig() *packet.Config {
	return &packet.Config{DefaultHash: crypto.SHA256}
}

// ParsePGPKey parses an ASCII-armored OpenPGP private key. The key
// must not be passphrase protected.
func ParsePGPKey(armoredPrivate string) (*PGPKey, error) {
	armoredPrivate = strings.TrimSpace(armoredPrivate)
	if armoredPrivate == "" {
		return nil, errors.New("signing: empty pgp key")
	}
	list, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredPrivate))
	if err != nil {
		return nil, fmt.Errorf("signing: read pgp key: %w", err)
	}
	for _, e := range list {
		if e.PrivateKey == nil {
			continue
		}
		if e.PrivateKey.Encrypted {
			return nil, errors.New("signing: pgp private key is passphrase protected")
		}
		return &PGPKey{entity: e}, nil
	}
	return nil, errors.New("signing: no pgp private key found")
}

// NewPGPKey wraps an existing entity (which must carry a private key).
func NewPGPKey(e *openpgp.Entity) *PGPKey { return &PGPKey{entity: e} }

// Entity returns the underlying OpenPGP entity.
func (k *PGPKey) Entity() *openpgp.Entity { return k.entity }

// ClearSign returns an inline clear-signed document (apt InRelease).
func (k *PGPKey) ClearSign(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, k.entity.PrivateKey, pgpConfig())
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DetachSignArmored returns an armored detached signature (Release.gpg,
// repomd.xml.asc).
func (k *PGPKey) DetachSignArmored(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, k.entity, bytes.NewReader(data), pgpConfig()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DetachSignBinary returns a binary detached signature.
func (k *PGPKey) DetachSignBinary(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := openpgp.DetachSign(&buf, k.entity, bytes.NewReader(data), pgpConfig()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PublicKeyArmored returns the armored public key block.
func (k *PGPKey) PublicKeyArmored() ([]byte, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := k.entity.Serialize(w); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// PublicKeyBinary returns the binary (dearmored) public key.
func (k *PGPKey) PublicKeyBinary() ([]byte, error) {
	var buf bytes.Buffer
	if err := k.entity.Serialize(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// KeyID returns the 16-char upper-case hex key id of the primary key.
func (k *PGPKey) KeyID() string {
	return fmt.Sprintf("%016X", k.entity.PrimaryKey.KeyId)
}

// ParseRSAKey parses a PEM RSA private key (PKCS#1 or PKCS#8).
func ParseRSAKey(pemPrivate string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemPrivate)))
	if block == nil {
		return nil, errors.New("signing: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("signing: parse rsa key: %w", err)
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("signing: PEM key is not RSA")
	}
	return k, nil
}

// RSAPublicPEM returns the PKIX PEM-encoded public key of k.
func RSAPublicPEM(k *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
