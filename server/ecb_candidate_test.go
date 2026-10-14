package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo2http"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func ecbCandidateFixture(t *testing.T) (ecbHostConfig, string) {
	t.Helper()
	data, err := os.ReadFile("testdata/ecb/accepted-b1-proposal.json")
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		Expected struct {
			Binding providerreads.Binding `json:"binding"`
			Right   license.SourceRight   `json:"right"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(data, &accepted); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ecb-http.yaml")
	if err := os.WriteFile(path, []byte(ecbHTTPManifest), 0600); err != nil {
		t.Fatal(err)
	}
	config := ecbHostConfig{
		Format: ecbHostFormat, PublisherManifest: "testdata/ecb/original-ovdb.yaml",
		HTTPManifest: path, PublisherCommit: ecbPublisherCommit, PublisherBlob: ecbPublisherBlob,
		DecoderVersion: "ecb-eurofxref/1", DecoderModule: "v0.3.0",
		SourceRight: accepted.Expected.Right, Binding: accepted.Expected.Binding,
	}
	return config, filepath.Join(t.TempDir(), "operator.json")
}

func writeECBConfig(t *testing.T, path string, config ecbHostConfig) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func syntheticECBMount(t *testing.T, reads *int) ecbMount {
	return syntheticECBMountWithTransport(t, probeTransport(func(r *http.Request) (*http.Response, error) {
		*reads++
		if r.URL.String() != manifest.ECBDailyURL || r.Method != http.MethodGet || r.Header.Get("Cache-Control") != "no-store, no-cache" {
			return nil, errors.New("unexpected synthetic upstream request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(syntheticBody))}, nil
	}))
}

func syntheticECBMountWithTransport(t *testing.T, transport http.RoundTripper) ecbMount {
	t.Helper()
	return func(path string) (*core.Database, error) {
		m, err := manifest.Load(path)
		if err != nil {
			return nil, err
		}
		driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive,
			Client: &http.Client{Transport: transport},
			Collections: []dalgo2http.Collection{{Name: "daily", URLTemplate: manifest.ECBDailyURL,
				Decoder: dalgo2http.DecoderECBEuroFXRef, KeyField: "currency", Timeout: 10 * time.Second, ClientSideFilter: true}},
		})
		if err != nil {
			return nil, err
		}
		return core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	}
}

func ecbRequest(h http.Handler, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/databases/ecb/dtql", strings.NewReader(query))
	r.Header.Set(server.ProviderExecutionIDHeader, strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestECBHostCandidateSyntheticShippingAssembly(t *testing.T) {
	config, path := ecbCandidateFixture(t)
	writeECBConfig(t, path, config)
	reads := 0
	var diagnostics bytes.Buffer
	h, closeHandler, err := assembleECBHostCandidate(path, syntheticECBMount(t, &reads), &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	if reads != 0 {
		t.Fatal("candidate startup fetched provider")
	}
	query := "from: {name: daily}\ncolumns: [{field: time}, {field: currency}, {field: rate}]\nlimit: 1\n"
	w := ecbRequest(h, query)
	if w.Code != http.StatusOK || reads != 1 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("candidate read: status=%d reads=%d cache=%q", w.Code, reads, w.Header().Get("Cache-Control"))
	}
	var result struct {
		Records       []json.RawMessage     `json:"records"`
		SourceRights  []license.SourceRight `json:"sourceRights"`
		ProviderReads json.RawMessage       `json:"providerReads"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Records) != 1 || len(result.SourceRights) != 1 ||
		result.SourceRights[0].SourceID != "ovdb:openvaultdb-cloud/ecb/daily" || !bytes.Contains(result.Records[0], []byte("001.23000")) {
		t.Fatalf("candidate response lacks bound native evidence: %v", err)
	}
	evidence, err := providerreads.Decode(result.ProviderReads)
	if err != nil || evidence.Execution.ExecutorID != "openvaultdb-cloud" || evidence.Execution.ID != strings.Repeat("a", 32) || len(evidence.Reads) != 1 {
		t.Fatalf("candidate evidence: %v", err)
	}
	for _, forbidden := range []string{
		"from: {name: daily}\nlimit: 51\n",
		"from: {name: daily}\norderBy: [{field: rate}]\nlimit: 1\n",
		"from: {name: history}\nlimit: 1\n",
	} {
		w = ecbRequest(h, forbidden)
		if w.Code == http.StatusOK || reads != 1 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("forbidden candidate request: status=%d reads=%d", w.Code, reads)
		}
	}
	if diagnostics.Len() != 0 {
		t.Fatal("candidate logged a normal or refused read")
	}
}

func TestECBHostCandidateDriftRefusesBeforeMount(t *testing.T) {
	base, path := ecbCandidateFixture(t)
	for name, mutate := range map[string]func(*ecbHostConfig){
		"publisher commit": func(c *ecbHostConfig) { c.PublisherCommit = strings.Repeat("0", 40) },
		"publisher blob":   func(c *ecbHostConfig) { c.PublisherBlob = strings.Repeat("0", 40) },
		"decoder":          func(c *ecbHostConfig) { c.DecoderVersion = "other" },
		"decoder module":   func(c *ecbHostConfig) { c.DecoderModule = "v0.2.0" },
		"right":            func(c *ecbHostConfig) { c.SourceRight.Attribution.Text = "changed" },
		"binding":          func(c *ecbHostConfig) { c.Binding.DecoderDigest = strings.Repeat("0", 64) },
		"publisher bytes":  func(c *ecbHostConfig) { c.PublisherManifest = c.HTTPManifest },
		"storage bytes":    func(c *ecbHostConfig) { c.HTTPManifest = c.PublisherManifest },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			writeECBConfig(t, path, config)
			mounts := 0
			if h, closeHandler, err := assembleECBHostCandidate(path, func(string) (*core.Database, error) { mounts++; return nil, nil }, io.Discard); err == nil || h != nil || closeHandler != nil || mounts != 0 {
				t.Fatalf("drift crossed mount: mounts=%d err=%v", mounts, err)
			}
		})
	}
}

func TestECBCandidateErrorMarkerDiscardsPayload(t *testing.T) {
	var sink bytes.Buffer
	logger := slog.New(&ecbMarkerHandler{out: &sink})
	logger.ErrorContext(context.Background(), "synthetic-body-marker", "row", "synthetic-row-marker", "error", errors.New("synthetic-error-marker"))
	if got := sink.String(); got != "ecb_candidate_internal_error\n" {
		t.Fatalf("unfixed candidate diagnostic: %q", got)
	}
}

func TestECBHostCandidateTransportErrorMarker(t *testing.T) {
	config, path := ecbCandidateFixture(t)
	writeECBConfig(t, path, config)
	var diagnostics bytes.Buffer
	reads := 0
	h, closeHandler, err := assembleECBHostCandidate(path, syntheticECBMountWithTransport(t, probeTransport(func(*http.Request) (*http.Response, error) {
		reads++
		return nil, errors.New("synthetic-upstream-private-marker")
	})), &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	w := ecbRequest(h, "from: {name: daily}\nlimit: 1\n")
	if reads != 1 || w.Code == http.StatusOK || strings.Contains(w.Body.String(), "synthetic-upstream-private-marker") ||
		strings.Contains(diagnostics.String(), "synthetic-upstream-private-marker") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("private transport marker escaped or request did not reach synthetic transport: status=%d reads=%d", w.Code, reads)
	}
}
