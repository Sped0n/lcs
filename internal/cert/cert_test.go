package cert

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorityPersistence(t *testing.T) {
	state := t.TempDir()
	first, err := Ensure(state)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate.Raw, second.Certificate.Raw) {
		t.Fatal("CA changed")
	}
	info, err := os.Stat(filepath.Join(state, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("unsafe CA key permissions")
	}
	leaf, err := first.Leaf("files.pythonhosted.org")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(first.Certificate)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "files.pythonhosted.org"}); err != nil {
		t.Fatal(err)
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "other.test"}); err == nil {
		t.Fatal("leaf valid for wrong host")
	}
}
