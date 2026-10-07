package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
)

const syntheticIANASecret = "synthetic-iana-proxy-secret-32-bytes-long"
const syntheticIANACSV = "Value,Description,Reference\n799,Invented status,[Invented]\n800-899,Invented range,[Invented]\n"

func syntheticIANAMount(t *testing.T, transport http.RoundTripper) ecbMount {
	t.Helper()
	return func(path string) (*core.Database, error) {
		m, err := manifest.Load(path)
		if err != nil {
			return nil, err
		}
		driver, err := dalgo2http.NewDB(dalgo2http.Config{Mode: dalgo2http.ModeLive,
			Client: &http.Client{Transport: transport},
			Collections: []dalgo2http.Collection{{Name: "rows", URLTemplate: manifest.IANAHTTPStatusURL,
				Decoder: dalgo2http.DecoderStrictCSV3, KeyField: "Value", Timeout: 10 * time.Second, ClientSideFilter: true}},
		})
		if err != nil {
			return nil, err
		}
		return core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	}
}

func writeIANAJSON(t *testing.T, path string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func configureSyntheticIANA(t *testing.T, open ecbMount) (string, ianaHostConfig, ianaAdmission) {
	t.Helper()
	dir := t.TempDir()
	metadata := []byte("synthetic publisher definition; not a captured IANA response\n")
	definition := filepath.Join(dir, "synthetic-definition.txt")
	transport := filepath.Join(dir, "transport.yaml")
	for path, data := range map[string][]byte{definition: metadata, transport: []byte(ianaHTTPManifest)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Obtain the library's mounted declaration, then attach synthetic notices/pins.
	db, err := syntheticIANAMount(t, probeTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("fixture preparation must not fetch")
		return nil, errors.New("unexpected read")
	}))(transport)
	if err != nil {
		t.Fatal(err)
	}
	right, err := db.SourceRight("openvaultdb-cloud", nil, "rows")
	_ = db.Close()
	if err != nil || right == nil {
		t.Fatal(err)
	}
	definitionSHA := fmt.Sprintf("%x", sha256.Sum256(metadata))
	right.EvidenceOrigin = "publisher-definition-verified"
	right.Pins = []license.Pin{{Role: "provider", Repository: "https://github.com/synthetic/provider", Revision: strings.Repeat("c", 40), Path: "synthetic-definition.txt", SHA256: definitionSHA, Bytes: int64(len(metadata))}}
	right.Attribution = &license.Notice{Text: "Synthetic registry attribution", URL: "https://example.org/"}
	right.FreeSource = &license.Notice{Text: "Synthetic original", URL: manifest.IANAHTTPStatusURL}
	right.Transformations = []string{"Synthetic CSV parsed into lexical strings"}
	rightsSHA, err := providerreads.RightsDigest(*right)
	if err != nil {
		t.Fatal(err)
	}
	config := ianaHostConfig{Format: "ovdb-iana-host-candidate/1", PublisherDefinition: definition, HTTPManifest: transport,
		DecoderVersion: "strict-csv-three-column/1", DecoderModuleVersion: "v0.4.0", SourceRight: *right,
		Binding: providerreads.Binding{ProviderSourceID: "provider:iana/HttpStatusRegistryRow", RightsSourceID: right.SourceID,
			ResourceID: "iana-http-status-codes", DefinitionDigest: definitionSHA, DecoderDigest: strings.Repeat("b", 64), RightsDigest: rightsSHA}}
	configPath := filepath.Join(dir, "host.json")
	admission := ianaAdmission{Format: "ovdb-iana-operator-admission/1", Decision: "operator-free-transient-read-only",
		ApprovedBy: "synthetic-test-operator", ApprovedAt: "2026-10-07T00:00:00Z",
		HostConfigSHA256: writeIANAJSON(t, configPath, config), DefinitionSHA256: definitionSHA,
		DecoderSHA256: config.Binding.DecoderDigest, RightsSHA256: rightsSHA, ExecutorID: "openvaultdb-cloud",
		ResourceID: "iana-http-status-codes", Method: http.MethodPost, Path: ianaQueryPath, MaxReadsPerExecution: 1, MaxRows: 50}
	admissionPath := filepath.Join(dir, "admission.json")
	t.Setenv("OVDB_IANA_ADMISSION_SHA256", writeIANAJSON(t, admissionPath, admission))
	t.Setenv("OVDB_IANA_ADMISSION_FILE", admissionPath)
	t.Setenv("OVDB_IANA_HOST_CONFIG", configPath)
	t.Setenv("OVDB_IANA_PROXY_SECRET", syntheticIANASecret)
	t.Setenv("OVDB_IANA_ENABLED", "true")
	previous := selectedIANAMount
	selectedIANAMount = open
	t.Cleanup(func() { selectedIANAMount = previous })
	inventory, _ := selectedInventoryFixture(t)
	t.Setenv("SAMPLE_DATABASES_INVENTORY", inventory)
	t.Setenv("OVDB_SELECTED_STORAGE", string(sealedCopy))
	return configPath, config, admission
}

func ianaRequest(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, ianaQueryPath, strings.NewReader(body))
	r.Header.Set(ianaProxySecretHeader, syntheticIANASecret)
	r.Header.Set("OVDB-Execution-ID", strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestConfiguredHandlerIANASelectedQuery(t *testing.T) {
	reads := 0
	configureSyntheticIANA(t, syntheticIANAMount(t, probeTransport(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.URL.String() != manifest.IANAHTTPStatusURL || r.Method != http.MethodGet || r.Header.Get("Cache-Control") != "no-store, no-cache" {
			t.Fatal("unexpected upstream request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/csv"}}, Body: io.NopCloser(strings.NewReader(syntheticIANACSV))}, nil
	})))
	t.Setenv("OVDB_IANA_ENABLED", "")
	_, off, closeOff, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	if ianaRequest(off, "from: {name: rows}\nlimit: 1\n").Code == 200 || reads != 0 {
		t.Fatal("config alone selected IANA")
	}
	_ = closeOff()
	t.Setenv("OVDB_IANA_ENABLED", "true")
	_, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	if reads != 0 {
		t.Fatal("startup read provider")
	}
	for _, secrets := range [][]string{nil, {"wrong"}, {syntheticIANASecret, syntheticIANASecret}} {
		r := httptest.NewRequest(http.MethodPost, ianaQueryPath, strings.NewReader("from: {name: rows}\nlimit: 1\n"))
		for _, s := range secrets {
			r.Header.Add(ianaProxySecretHeader, s)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 404 || reads != 0 {
			t.Fatal("secret refusal crossed provider")
		}
	}
	for _, tc := range []struct{ method, path string }{{"GET", ianaQueryPath}, {"POST", ianaQueryPath + "?"}, {"POST", ianaQueryPath + "?x=1"}, {"POST", "/v1/databases/iana-http-status/%64tql"}, {"GET", "/v1/databases/iana-http-status/records/rows/799"}} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("from: {name: rows}\nlimit: 1\n"))
		r.Header.Set(ianaProxySecretHeader, syntheticIANASecret)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code == 200 || reads != 0 {
			t.Fatal("alternate path read provider")
		}
	}
	for _, query := range []string{"from: {name: rows}\nlimit: 51\n", "from: {name: rows}\ncolumns: [{field: statusCode}]\nlimit: 1\n", "from: {name: rows}\nlimit: 1\norderBy: [{field: Value}]\n"} {
		if ianaRequest(handler, query).Code == 200 || reads != 0 {
			t.Fatal("unsupported query crossed provider")
		}
	}
	query := "from: {name: rows}\ncolumns: [{field: Value}, {field: Description}]\nwhere: {op: '==', left: {field: Value}, right: {value: 800-899}}\nlimit: 1\n"
	for i := 1; i <= 2; i++ {
		w := ianaRequest(handler, query)
		if w.Code != 200 || reads != i || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"Value":"800-899"`) {
			t.Fatalf("native query: %d reads=%d %s", w.Code, reads, w.Body)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/openvaultdb", nil))
	if w.Code != 200 || reads != 2 {
		t.Fatal("sample discovery changed")
	}
}

func TestConfiguredHandlerIANAAdmissionDriftRefusesBeforeMount(t *testing.T) {
	for _, name := range []string{"missing admission", "bad digest", "duplicate key", "unknown member", "trailing document", "host drift", "definition drift", "public", "paid", "reads", "rows", "executor", "binding", "rights", "transport", "decoder", "flag", "secret"} {
		t.Run(name, func(t *testing.T) {
			mounts := 0
			path, config, a := configureSyntheticIANA(t, func(string) (*core.Database, error) { mounts++; return nil, errors.New("private-mount-marker") })
			ap := os.Getenv("OVDB_IANA_ADMISSION_FILE")
			switch name {
			case "missing admission":
				t.Setenv("OVDB_IANA_ADMISSION_FILE", "")
			case "bad digest":
				t.Setenv("OVDB_IANA_ADMISSION_SHA256", strings.Repeat("0", 64))
			case "duplicate key", "unknown member", "trailing document":
				data, _ := os.ReadFile(ap)
				if name == "duplicate key" {
					data = bytes.Replace(data, []byte(`"publicAccess":false`), []byte(`"publicAccess":false,"publicAccess":false`), 1)
				}
				if name == "unknown member" {
					data = bytes.Replace(data, []byte(`"publicAccess":false`), []byte(`"unexpected":false,"publicAccess":false`), 1)
				}
				if name == "trailing document" {
					data = append(data, []byte("{}")...)
				}
				if err := os.WriteFile(ap, data, 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("OVDB_IANA_ADMISSION_SHA256", fmt.Sprintf("%x", sha256.Sum256(data)))
			case "host drift":
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "definition drift":
				if err := os.WriteFile(config.PublisherDefinition, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "public":
				a.PublicAccess = true
			case "paid":
				a.PaidAccess = true
			case "reads":
				a.MaxReadsPerExecution = 2
			case "rows":
				a.MaxRows = 51
			case "executor":
				a.ExecutorID = "other"
			case "binding":
				config.Binding.ProviderSourceID = "provider:other/Row"
			case "rights":
				config.SourceRight.Transformations = []string{"changed"}
			case "transport":
				if err := os.WriteFile(config.HTTPManifest, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "decoder":
				config.DecoderVersion = "other/1"
			case "flag":
				t.Setenv("OVDB_IANA_ENABLED", "yes")
			case "secret":
				t.Setenv("OVDB_IANA_PROXY_SECRET", "short")
			}
			if name == "binding" || name == "rights" || name == "decoder" {
				a.HostConfigSHA256 = writeIANAJSON(t, path, config)
			}
			if name == "public" || name == "paid" || name == "reads" || name == "rows" || name == "executor" || name == "binding" || name == "rights" || name == "decoder" {
				t.Setenv("OVDB_IANA_ADMISSION_SHA256", writeIANAJSON(t, ap, a))
			}
			_, h, closeHandler, err := configuredHandler()
			if err == nil || h != nil || closeHandler != nil || mounts != 0 || strings.Contains(err.Error(), "private-mount-marker") {
				t.Fatalf("invalid config reached mount: %d %v", mounts, err)
			}
		})
	}
}

func TestConfiguredHandlerIANAFailureDiscardsPayload(t *testing.T) {
	const marker = "synthetic-private-marker"
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			reads := 0
			var logs bytes.Buffer
			configureSyntheticIANA(t, syntheticIANAMount(t, probeTransport(func(*http.Request) (*http.Response, error) {
				reads++
				if !malformed {
					return nil, errors.New(marker)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/csv"}}, Body: io.NopCloser(strings.NewReader("Value,Description,Reference\n799," + marker + ",x\n799,duplicate,x\n"))}, nil
			})))
			prior := selectedIANADiagnostic
			selectedIANADiagnostic = &logs
			t.Cleanup(func() { selectedIANADiagnostic = prior })
			_, h, closeHandler, err := configuredHandler()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeHandler() })
			slog.New(&ianaMarkerHandler{out: &logs}).With("payload", marker).Error(marker)
			if logs.String() != "iana_candidate_internal_error\n" {
				t.Fatal("log control failed")
			}
			w := ianaRequest(h, "from: {name: rows}\nlimit: 1\n")
			if w.Code == 200 || reads != 1 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String()+logs.String(), marker) {
				t.Fatalf("failure leaked: %d reads=%d %s", w.Code, reads, w.Body)
			}
		})
	}
}

func TestConfiguredHandlerIANAAndECBStayIndependent(t *testing.T) {
	ecbReads, ianaReads := 0, 0
	configureSyntheticECB(t, syntheticECBMount(t, &ecbReads))
	t.Setenv("OVDB_ECB_ENABLED", "true")
	configureSyntheticIANA(t, syntheticIANAMount(t, probeTransport(func(*http.Request) (*http.Response, error) {
		ianaReads++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/csv"}}, Body: io.NopCloser(strings.NewReader(syntheticIANACSV))}, nil
	})))
	_, handler, closeHandler, err := configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeHandler() })
	if w := selectedECBRequest(handler, "from: {name: daily}\nlimit: 1\n"); w.Code != 200 || ecbReads != 1 || ianaReads != 0 {
		t.Fatalf("ECB composition changed: %d %s", w.Code, w.Body)
	}
	if w := ianaRequest(handler, "from: {name: rows}\nlimit: 1\n"); w.Code != 200 || ecbReads != 1 || ianaReads != 1 {
		t.Fatalf("IANA composition changed: %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest(http.MethodPost, ianaQueryPath, strings.NewReader("from: {name: rows}\nlimit: 1\n"))
	r.Header.Set(ecbProxySecretHeader, syntheticECBProxySecret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 404 || ianaReads != 1 || ecbReads != 1 {
		t.Fatal("ECB secret selected IANA")
	}
}

func TestConfiguredHandlerIANARejectsMountedManifestSwap(t *testing.T) {
	reads := 0
	open := syntheticIANAMount(t, probeTransport(func(*http.Request) (*http.Response, error) { reads++; return nil, errors.New("unexpected read") }))
	configureSyntheticIANA(t, func(path string) (*core.Database, error) {
		db, err := open(path)
		if err == nil {
			db.Manifest.Database.License.Name = "changed after pin check"
		}
		return db, err
	})
	_, handler, closeHandler, err := configuredHandler()
	if err == nil || handler != nil || closeHandler != nil || reads != 0 {
		t.Fatal("manifest swap admitted")
	}
}

func TestConfiguredHandlerIANAPublisherHTMLDefinition(t *testing.T) {
	for _, drift := range []string{"", "publisher", "directory", "mixed origin"} {
		t.Run(drift, func(t *testing.T) {
			reads := 0
			path, config, admission := configureSyntheticIANA(t, syntheticIANAMount(t, probeTransport(func(*http.Request) (*http.Response, error) {
				reads++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/csv"}}, Body: io.NopCloser(strings.NewReader(syntheticIANACSV))}, nil
			})))
			config.DirectoryDefinition = config.PublisherDefinition
			config.PublisherDefinition = filepath.Join(filepath.Dir(path), "publisher-html.json")
			config.SourceRight.EvidenceOrigin = "publisher-html-metadata-verified"
			config.SourceRight.Pins[0].Role = "discovery"
			config.SourceRight.Pins[0].Repository = "https://github.com/openvaultdb/directory"
			config.SourceRight.Pins[0].Path = "sources/$records/iana-http-status-codes.yaml"
			config.SourceRight.PublisherHTMLDefinition = &license.PublisherHTMLDefinition{
				Format: "ovdb-iana-publisher-html-definition/1", RegistryURL: "https://www.iana.org/assignments/http-status-codes",
				RegistrySHA256: strings.Repeat("d", 64), RegistryBytes: 100, TermsURL: manifest.IANALicensingTermsURL,
				TermsSHA256: strings.Repeat("e", 64), TermsBytes: 200, ObservedAt: "2026-10-07T00:00:00Z",
				ResourceURL: manifest.IANAHTTPStatusURL, NativeFields: []string{"Value", "Description", "Reference"},
				RightsScope: "iana-ietf-held-protocol-registry-rights-cc0-excluding-linked-material",
			}
			definition, err := providerreads.Canonical(config.SourceRight.PublisherHTMLDefinition)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(config.PublisherDefinition, definition, 0600); err != nil {
				t.Fatal(err)
			}
			admission.DefinitionSHA256 = fmt.Sprintf("%x", sha256.Sum256(definition))
			config.Binding.DefinitionDigest = admission.DefinitionSHA256
			admission.RightsSHA256, err = providerreads.RightsDigest(config.SourceRight)
			if err != nil {
				t.Fatal(err)
			}
			config.Binding.RightsDigest = admission.RightsSHA256
			if drift == "mixed origin" {
				config.SourceRight.EvidenceOrigin = "publisher-definition-verified"
			}
			admission.HostConfigSHA256 = writeIANAJSON(t, path, config)
			t.Setenv("OVDB_IANA_ADMISSION_SHA256", writeIANAJSON(t, os.Getenv("OVDB_IANA_ADMISSION_FILE"), admission))
			if drift == "publisher" || drift == "directory" {
				changed := config.PublisherDefinition
				if drift == "directory" {
					changed = config.DirectoryDefinition
				}
				if err := os.WriteFile(changed, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, handler, closeHandler, err := configuredHandler()
			if drift != "" {
				if err == nil || handler != nil || closeHandler != nil || reads != 0 {
					t.Fatal("mixed or changed metadata admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = closeHandler() })
			if reads != 0 {
				t.Fatal("metadata startup read provider")
			}
			if result := ianaRequest(handler, "from: {name: rows}\nlimit: 1\n"); result.Code != 200 || reads != 1 {
				t.Fatalf("distinct metadata execution: %d reads=%d", result.Code, reads)
			}
		})
	}
}
