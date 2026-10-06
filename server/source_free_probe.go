package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

const syntheticBody = `<g:Envelope xmlns:g="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref"><g:subject>Synthetic</g:subject><g:Sender><g:name>Test</g:name></g:Sender><Cube><Cube time="2037-02-03"><Cube currency="AAA" rate="001.23000"/></Cube></Cube></g:Envelope>`

type probeTransport func(*http.Request) (*http.Response, error)

func (f probeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Process-only diagnostics. Neither command opens a listener or registers a
// profile with the public inventory. Synthetic dynamic admission is deliberately
// separate from the immutable production inventory and asserts no real rights.
func runSourceFreeProbe(args []string, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "--synthetic-dynamic-probe":
		return syntheticDynamicProbe(out)
	case len(args) == 1 && args[0] == "--roots-probe":
		roots, pool, err := probeRoots()
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"probe": "roots/1", "outcome": "pass", "rootsSha256": digest(roots), "certificates": len(pool.Subjects())})
	case len(args) == 3 && args[0] == "--https-trust-probe":
		return httpsTrustProbe(args[1], args[2], out)
	default:
		return errors.New("unsupported probe command")
	}
}

func newSyntheticDynamicHandler(transport http.RoundTripper, mutate func(*server.ProviderReadProfile)) (http.Handler, func() error, error) {
	driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive,
		Client:      &http.Client{Transport: transport},
		Collections: []dalgo2http.Collection{{Name: "daily", URLTemplate: manifest.ECBDailyURL, Decoder: dalgo2http.DecoderECBEuroFXRef, KeyField: "currency", Timeout: 10 * time.Second, ClientSideFilter: true}},
	})
	if err != nil {
		return nil, nil, err
	}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "synthetic-dynamic", SchemaMode: schema.ModeStrict, License: &license.Declaration{URL: "https://example.org/synthetic-terms", Text: "Authored synthetic probe only"}},
		Storage:  manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "daily"}},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"daily": {Fields: map[string]schema.Field{"time": {Type: schema.TypeString}, "currency": {Type: schema.TypeString}, "rate": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (http.Handler, func() error, error) { _ = db.Close(); return nil, nil, err }
	right, err := db.SourceRight("synthetic-cloud-probe", nil, "daily")
	if err != nil || right == nil {
		return fail(errors.New("synthetic rights absent"))
	}
	right.EvidenceOrigin = "publisher-definition-verified"
	right.Pins = []license.Pin{{Role: "provider", Repository: "https://github.com/synthetic/provider", Revision: strings.Repeat("c", 40), Path: "synthetic.json", SHA256: strings.Repeat("a", 64), Bytes: 42}}
	right.Attribution = &license.Notice{Text: "Authored synthetic provider", URL: "https://example.org/"}
	right.FreeSource = &license.Notice{Text: "Synthetic original", URL: manifest.ECBDailyURL}
	right.Transformations = []string{"Authored synthetic XML restructured into rows"}
	rightsDigest, err := providerreads.RightsDigest(*right)
	if err != nil {
		return fail(err)
	}
	profile := server.ProviderReadProfile{Collection: "daily", SourceRight: right, Binding: providerreads.Binding{ProviderSourceID: "provider:synthetic/FxReferenceQuote", RightsSourceID: right.SourceID, ResourceID: "ecb-daily", DefinitionDigest: strings.Repeat("a", 64), DecoderDigest: strings.Repeat("b", 64), RightsDigest: rightsDigest}}
	if mutate != nil {
		mutate(&profile)
	}
	if profile.SourceRight == nil {
		return fail(errors.New("synthetic dynamic notices missing"))
	}
	s, err := server.NewChecked("synthetic-cloud-probe", map[string]*core.Database{"synthetic-dynamic": db},
		server.WithReadOnly(true), server.WithSourceRights("synthetic-cloud-probe", nil),
		server.WithProviderReadProfiles(map[string]server.ProviderReadProfile{"synthetic-dynamic": profile}),
		server.WithCORS(server.ParseCORSOrigins([]string{"https://synthetic.example"})),
	)
	if err != nil {
		return fail(err)
	}
	return s.Handler(), func() error { s.CloseSnapshots(); return db.Close() }, nil
}

func syntheticDynamicProbe(out io.Writer) error {
	calls := 0
	h, closeHandler, err := newSyntheticDynamicHandler(probeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != manifest.ECBDailyURL || r.Method != "GET" || r.Header.Get("Cache-Control") != "no-store, no-cache" {
			return nil, errors.New("synthetic transport mismatch")
		}
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(syntheticBody))}, nil
	}), nil)
	if err != nil {
		return err
	}
	defer closeHandler()
	id := strings.Repeat("d", 32)
	r := httptest.NewRequest("POST", "/v1/databases/synthetic-dynamic/query", strings.NewReader(`{"collection":"daily"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(server.ProviderExecutionIDHeader, id)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var result struct {
		Evidence json.RawMessage `json:"providerReads"`
		Records  []any           `json:"records"`
	}
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || calls != 1 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Records) != 1 {
		return errors.New("synthetic query failed")
	}
	evidence, err := providerreads.Decode(result.Evidence)
	if err != nil || evidence.Execution.ID != id || len(evidence.Reads) != 1 {
		return errors.New("synthetic evidence failed")
	}
	// Receipts contain counters and digests only; result/source bytes stay transient.
	return json.NewEncoder(out).Encode(map[string]any{"probe": "synthetic-dynamic/1", "outcome": "pass", "publicAdmission": false, "networkCalls": 0, "syntheticReads": calls, "evidenceSha256": digest(result.Evidence)})
}

// An operator supplies an authored endpoint and expected digest; the mandatory
// path prevents accidental use as a generic data-fetch command. This does not
// execute the guarded provider mount or certify its production startup.
func httpsTrustProbe(rawURL, expected string, out io.Writer) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "/ovdb-synthetic-trust-probe" || u.RawQuery != "" || u.Fragment != "" || strings.EqualFold(u.Hostname(), "www.ecb.europa.eu") || !providerSHA256Pattern.MatchString(expected) {
		return errors.New("invalid synthetic trust endpoint")
	}
	roots, pool, err := probeRoots()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Cache-Control", "no-store, no-cache")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	// Normal TLS/hostname validation against the exact receipt's root file. This
	// uses Go's standard transport, never the synthetic dynamic RoundTripper.
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	response, err := client.Do(request)
	if err != nil {
		var authority x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		if errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
			_ = json.NewEncoder(out).Encode(map[string]any{"probe": "https-trust/1", "outcome": "refused", "reason": "tls_validation_failed", "rootsSha256": digest(roots)})
		}
		return errors.New("synthetic TLS request failed")
	}
	defer response.Body.Close()
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(response.Body, 4097))
	if err != nil || response.StatusCode != 200 || n > 4096 || hex.EncodeToString(hasher.Sum(nil)) != expected {
		return errors.New("synthetic TLS response mismatch")
	}
	// No URL, certificate identity, body, rows or arbitrary transport error text.
	return json.NewEncoder(out).Encode(map[string]any{"probe": "https-trust/1", "outcome": "pass", "rootsSha256": digest(roots), "bytes": n, "bodySha256": expected})
}

func probeRoots() ([]byte, *x509.CertPool, error) {
	rootsPath := os.Getenv("SSL_CERT_FILE")
	if rootsPath == "" {
		rootsPath = "/etc/ssl/certs/ca-certificates.crt"
	}
	roots, err := os.ReadFile(rootsPath)
	if err != nil || len(roots) == 0 {
		return nil, nil, errors.New("trusted roots unreadable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		return nil, nil, errors.New("trusted roots invalid")
	}
	return roots, pool, nil
}
