package signing

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// TestArmoredKey generates a throwaway armored private key.
func testArmoredKey(t *testing.T) string {
	t.Helper()
	e, err := openpgp.NewEntity("kutu test", "", "test@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.String()
}

func TestPGP(t *testing.T) {
	k, err := ParsePGPKey(testArmoredKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if id := k.KeyID(); len(id) != 16 || strings.ToUpper(id) != id {
		t.Fatalf("key id %q", id)
	}
	pub, err := k.PublicKeyArmored()
	if err != nil {
		t.Fatal(err)
	}
	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pub))
	if err != nil {
		t.Fatal(err)
	}
	if ring[0].PrivateKey != nil {
		t.Fatal("public key export leaked private key")
	}
	data := []byte("Origin: kutu\nSuite: stable\n")

	cs, err := k.ClearSign(data)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := clearsign.Decode(cs)
	if b == nil {
		t.Fatal("clearsign decode failed")
	}
	if _, err := b.VerifySignature(openpgp.EntityList(ring), nil); err != nil {
		t.Fatalf("clearsign verify: %v", err)
	}
	if !bytes.Equal(b.Plaintext, bytes.TrimRight(data, "\n")) && !bytes.Equal(b.Plaintext, data) {
		t.Fatalf("plaintext mismatch: %q", b.Plaintext)
	}

	as, err := k.DetachSignArmored(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(ring, bytes.NewReader(data), bytes.NewReader(as), nil); err != nil {
		t.Fatalf("armored verify: %v", err)
	}
	bs, err := k.DetachSignBinary(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckDetachedSignature(ring, bytes.NewReader(data), bytes.NewReader(bs), nil); err != nil {
		t.Fatalf("binary verify: %v", err)
	}
	if _, err := openpgp.CheckDetachedSignature(ring, bytes.NewReader([]byte("tampered")), bytes.NewReader(bs), nil); err == nil {
		t.Fatal("tampered data verified")
	}
}

func TestParsePGPKeyErrors(t *testing.T) {
	if _, err := ParsePGPKey(""); err == nil {
		t.Fatal("expected error for empty key")
	}
	k, _ := ParsePGPKey(testArmoredKey(t))
	pub, _ := k.PublicKeyArmored()
	if _, err := ParsePGPKey(string(pub)); err == nil {
		t.Fatal("expected error for public-only key")
	}
}

func TestRSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for _, p := range [][]byte{pkcs1, pkcs8} {
		k, err := ParseRSAKey(string(p))
		if err != nil {
			t.Fatal(err)
		}
		if !k.Equal(priv) {
			t.Fatal("key mismatch")
		}
	}
	pub, err := RSAPublicPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pub), "BEGIN PUBLIC KEY") {
		t.Fatalf("bad pub pem %s", pub)
	}
	if _, err := ParseRSAKey("junk"); err == nil {
		t.Fatal("expected error")
	}
}
