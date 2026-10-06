package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func TestSourceFreeDynamicProbe(t *testing.T) {
	var out bytes.Buffer
	if err := runSourceFreeProbe([]string{"--synthetic-dynamic-probe"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"networkCalls":0`) || strings.Contains(out.String(), "001.23000") || strings.Contains(out.String(), "Envelope") {
		t.Fatal("unsafe receipt")
	}
}

func TestSyntheticAdmissionRejectsBeforeRead(t *testing.T) {
	for _, change := range []func(*server.ProviderReadProfile){
		func(p *server.ProviderReadProfile) { p.Binding.RightsDigest = strings.Repeat("e", 64) },
		func(p *server.ProviderReadProfile) { p.Binding.DefinitionDigest = strings.Repeat("e", 64) },
		func(p *server.ProviderReadProfile) { p.Collection = "missing" },
		func(p *server.ProviderReadProfile) { p.SourceRight.EvidenceOrigin = "unverified" },
		func(p *server.ProviderReadProfile) { p.SourceRight = nil },
	} {
		calls := 0
		h, closeHandler, err := newSyntheticDynamicHandler(probeTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected read") }), change)
		if closeHandler != nil {
			_ = closeHandler()
		}
		if h != nil || err == nil || calls != 0 {
			t.Fatal("invalid synthetic admission read or succeeded")
		}
	}
	// Production inventory rejects this profile before file inspection/mounting.
	if err := validateRuntimeMembers([]byte(`{"version":2,"databases":[{"readProfile":"ecb-daily/1"}]}`)); err == nil {
		t.Fatal("production dynamic profile admitted")
	}
}

type blockedProbeBody struct {
	ctx       context.Context
	begun     chan struct{}
	closed    chan struct{}
	once      sync.Once
	prefix    atomic.Bool
	closeOnce sync.Once
}

func (b *blockedProbeBody) Read(buffer []byte) (int, error) {
	if b.prefix.CompareAndSwap(false, true) {
		return copy(buffer, []byte("<g:Envelope SYNTHETIC_BLOCKED_PREFIX")), nil
	}
	b.once.Do(func() { close(b.begun) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *blockedProbeBody) Close() error { b.closeOnce.Do(func() { close(b.closed) }); return nil }

func TestSyntheticBlockedBodyBackendCancellation(t *testing.T) {
	// Owned finite test listener: httptest closes it via Cleanup. No provider I/O.
	begun, closed := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h, closeHandler, err := newSyntheticDynamicHandler(probeTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: &blockedProbeBody{ctx: r.Context(), begun: begun, closed: closed}}, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler()
	backend := httptest.NewServer(h)
	defer backend.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", backend.URL+"/v1/databases/synthetic-dynamic/query", strings.NewReader(`{"collection":"daily"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(server.ProviderExecutionIDHeader, strings.Repeat("d", 32))
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				err = errors.New("late success")
			}
		}
		done <- err
	}()
	select {
	case <-begun:
	case <-time.After(2 * time.Second):
		t.Fatal("body read did not begin")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("provider body did not close after cancellation")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled query succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not terminate")
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected read count")
	}
}

func TestHTTPSProbeValidationWithSyntheticRoots(t *testing.T) {
	// This verifies TLS behavior with an explicit test-root overlay, not the
	// shipping public-root image. Certificates are authored at runtime.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(t.TempDir(), "test-roots.pem")
	if err := os.WriteFile(rootFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", rootFile)
	for _, mode := range []string{"valid", "wrong-host", "expired", "untrusted", "redirect", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
			if mode == "wrong-host" {
				leaf.IPAddresses = nil
				leaf.DNSNames = []string{"wrong.invalid"}
			}
			if mode == "expired" {
				leaf.NotAfter = time.Now().Add(-time.Minute)
			}
			parent := ca
			if mode == "untrusted" {
				parent = leaf
			}
			certDER, err := x509.CreateCertificate(rand.Reader, leaf, parent, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, "/other", 302)
					return
				}
				if mode == "oversize" {
					_, _ = io.WriteString(w, strings.Repeat("x", 4097))
					return
				}
				_, _ = io.WriteString(w, "authored TLS probe")
			}))
			peer.Config.ErrorLog = log.New(io.Discard, "", 0)
			peer.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
			peer.StartTLS()
			defer peer.Close()
			var out bytes.Buffer
			err = httpsTrustProbe(peer.URL+"/ovdb-synthetic-trust-probe", digest([]byte("authored TLS probe")), &out)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("TLS outcome %s: %v", mode, err)
			}
			if strings.Contains(out.String(), "authored TLS probe") {
				t.Fatal("receipt contains body")
			}
			if (mode == "wrong-host" || mode == "expired" || mode == "untrusted") && !strings.Contains(out.String(), `"reason":"tls_validation_failed"`) {
				t.Fatal("non-TLS refusal cannot stand in for TLS verification")
			}
		})
	}
}
