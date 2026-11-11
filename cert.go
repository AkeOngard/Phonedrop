package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certValidity = 730 * 24 * time.Hour // 2 ปี
	certRenewAt  = 30 * 24 * time.Hour  // สร้างใหม่เมื่อเหลืออายุน้อยกว่านี้
)

// fingerprint: SHA-256 ของใบรับรอง ในรูป AB:CD:... ใช้เทียบกับที่มือถือแสดง
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// loadOrCreateCert โหลดใบรับรอง HTTPS ของเครื่องนี้ หรือสร้างใหม่ถ้ายังไม่มี/ใกล้หมดอายุ
// ใบรับรองเป็นแบบ self-signed: เข้ารหัสการส่งไฟล์ได้เต็มรูปแบบ แต่มือถือจะเตือนครั้งแรกเพราะไม่มีผู้รับรอง
// ใช้ใบเดิมไปตลอดเพื่อให้มือถือจำได้ — ถ้าวันหนึ่งมือถือเตือนซ้ำโดยไม่มีเหตุผล แปลว่าอาจมีคนแทรกกลาง
func loadOrCreateCert(dir string, ips []string) (tls.Certificate, string, error) {
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && time.Until(leaf.NotAfter) > certRenewAt {
			return c, fingerprint(leaf.Raw), nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PhoneDrop"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	return c, fingerprint(der), err
}
