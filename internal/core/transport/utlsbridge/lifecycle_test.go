package utlsbridge_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport/utlsbridge"
)

func TestBridgeCloseDoesNotHangOnStalledUpstream(t *testing.T) {
	addr, release, roots := stalledTLSServer(t)
	defer release()

	bridge, err := utlsbridge.NewWithConfig(utlsbridge.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := bridge.DialTLS(ctx, addr, "")
	if err != nil {
		bridge.Close()
		t.Fatalf("DialTLS: %v", err)
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		_ = bridge.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		release()
		conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("Bridge.Close hung on a stalled upstream")
	}
}

func stalledTLSServer(t *testing.T) (string, func(), *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	release := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				tlsConn, ok := conn.(*tls.Conn)
				if !ok {
					conn.Close()
					return
				}
				if err := tlsConn.Handshake(); err != nil {
					tlsConn.Close()
					return
				}
				<-release
				tlsConn.Close()
			}()
		}
	}()
	cleanup := func() {
		once.Do(func() {
			close(release)
			_ = ln.Close()
			wg.Wait()
		})
	}
	return ln.Addr().String(), cleanup, roots
}
