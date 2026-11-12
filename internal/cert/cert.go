package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

type Authority struct {
	Certificate *x509.Certificate
	key         crypto.Signer
}

func Load(state string) (*Authority, error) {
	crt, err := os.ReadFile(filepath.Join(state, "ca.crt"))
	if err != nil {
		return nil, err
	}
	keyPath := filepath.Join(state, "ca.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("ca.key must be readable only by its owner")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		return nil, fmt.Errorf("load CA: %w", err)
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if !certificate.IsCA || time.Now().After(certificate.NotAfter) {
		return nil, errors.New("CA is not valid; restore a valid CA before starting LCS")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("CA key cannot sign certificates")
	}
	return &Authority{Certificate: certificate, key: signer}, nil
}

// Ensure creates the key first so an interrupted creation never replaces an existing key.
// The caller holds the service's state-directory lock.
func Ensure(state string) (*Authority, error) {
	keyPath := filepath.Join(state, "ca.key")
	_, keyErr := os.Stat(keyPath)
	_, certErr := os.Stat(filepath.Join(state, "ca.crt"))
	if keyErr == nil && certErr == nil {
		return Load(state)
	}
	if keyErr != nil && !errors.Is(keyErr, os.ErrNotExist) {
		return nil, keyErr
	}
	if certErr != nil && !errors.Is(certErr, os.ErrNotExist) {
		return nil, certErr
	}
	if keyErr != nil && certErr == nil {
		return nil, errors.New("ca.key is missing; restore it instead of replacing the existing CA")
	}
	var key *ecdsa.PrivateKey
	var err error
	if keyErr == nil {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, errors.New("invalid ca.key")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		var ok bool
		key, ok = parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("unexpected CA key type")
		}
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := atomicWrite(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
			return nil, err
		}
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "LCS local CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(filepath.Join(state, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, err
	}
	return Load(state)
}

func Trusted(state string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(state, "ca.crt"))
	if err != nil {
		return false, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false, errors.New("invalid ca.crt")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, err
	}
	_, err = crt.Verify(x509.VerifyOptions{KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	if err != nil {
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) {
			return false, nil
		}
		return false, fmt.Errorf("verify LCS CA: %w", err)
	}
	return true, nil
}

func (a *Authority) Leaf(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, a.Certificate, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, a.Certificate.Raw}, PrivateKey: key}, nil
}

func atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ca-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("remove CA staging file: %v", err)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
