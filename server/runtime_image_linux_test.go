//go:build linux

package main

import (
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run only inside the finite server/test_image_runtime.py built-image experiment.
// It uses the production guard and checked handler; no listener or mock admission.
func TestProtectedImageLinuxJourney(t *testing.T) {
	mode := os.Getenv("OVDB_IMAGE_TEST")
	if mode == "" {
		t.Skip("requires the Linux image-runtime CI experiment; host tests are not image proof")
	}
	providers, handler, cleanup, err := configuredHandler()
	if mode == "refuse" {
		if err == nil || handler != nil || cleanup != nil {
			t.Fatal("hostile image returned a serving handler")
		}
		if expected := os.Getenv("OVDB_IMAGE_EXPECT"); expected == "" || !strings.Contains(err.Error(), expected) {
			t.Fatalf("wrong refusal: %v; want %s", err, expected)
		}
		t.Logf("refused before handler/listener: %v", err)
		return
	}
	if mode != "accept" {
		t.Fatal("unknown image experiment mode")
	}
	if err != nil {
		t.Fatal(err)
	}
	trust, err := x509.SystemCertPool()
	if err != nil || trust == nil {
		t.Fatalf("shipping image has no usable system CA trust store: pool=%v err=%v", trust != nil, err)
	}
	caBundle, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	if err != nil {
		t.Fatalf("shipping image has no readable system CA bundle: %v", err)
	}
	var validCertificates int
	for len(caBundle) > 0 {
		block, rest := pem.Decode(caBundle)
		if block == nil {
			break
		}
		caBundle = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err == nil {
			validCertificates++
		}
	}
	if validCertificates == 0 {
		t.Fatal("shipping image system CA bundle contains no valid certificates")
	}
	before, err := os.ReadDir("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	verifyReads := func() {
		t.Helper()
		nativeCount, selectedCount := 0, 0
		for _, provider := range providers {
			if provider.ReadProfile != "" {
				selectedCount += len(provider.SmokeRecordsets)
			}
			for _, table := range provider.SmokeRecordsets {
				profile := selectedRequest(handler, http.MethodGet, "/ovdb/dbs/"+provider.ID+"/collections/"+table, "")
				if profile.Code != http.StatusOK {
					t.Fatalf("schema/metadata: %d %s", profile.Code, profile.Body.String())
				}
				response := selectedRequest(handler, http.MethodPost, "/v1/databases/"+provider.ID+"/dtql", "from: {name: "+table+"}\nlimit: 100\n")
				var page northwindQueryPage
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Records) != 5 {
					t.Fatalf("ordinary read: %d %s", response.Code, response.Body.String())
				}
				for _, row := range page.Records {
					key := row.Data["native_key"].(string)
					read := selectedRequest(handler, http.MethodGet, "/v1/databases/"+provider.ID+"/records/"+table+"/"+url.PathEscape(key), "")
					if read.Code != 200 {
						t.Fatalf("key %q: %d %s", key, read.Code, read.Body.String())
					}
				}
			}
			path := filepath.Join(protectedFixtureRoot, provider.ID+".sqlite")
			raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
			if err != nil {
				t.Fatal(err)
			}
			// Empty idle pool forces real SQLite connections to reopen the OS-protected
			// path after each query; full handler remount below reopens both mount handles.
			raw.SetMaxIdleConns(0)
			for i := 0; i < 3; i++ {
				var count int
				if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil {
					t.Fatal(err)
				}
				if i == 0 && provider.ReadProfile != "" {
					nativeCount += count
				}
			}
			if _, err := raw.Exec("UPDATE table_00 SET native_key='changed' WHERE native_key='simple'"); err == nil {
				t.Fatal("OS-protected SQLite write succeeded")
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			for name, attempt := range map[string]func() error{
				"overwrite": func() error { return os.WriteFile(path, []byte("changed"), 0o444) },
				"truncate":  func() error { return os.Truncate(path, 0) },
				"rename":    func() error { return os.Rename(path, path+".renamed") },
				"unlink":    func() error { return os.Remove(path) },
				"replace":   func() error { return os.Symlink("/tmp/other.sqlite", path) },
			} {
				if err := attempt(); err == nil {
					t.Fatalf("protected asset %s succeeded", name)
				}
			}
			hash, err := sha256File(path)
			if err != nil || hash != provider.ServingSHA256 {
				t.Fatalf("asset changed: %s %v", provider.ID, err)
			}
		}
		if nativeCount != 16 || selectedCount != 11 {
			t.Fatalf("native/selected: %d/%d", nativeCount, selectedCount)
		}
		for _, table := range []string{"table_08", "table_09", "table_10", "table_11", "table_12"} {
			if response := selectedRequest(handler, http.MethodGet, "/ovdb/dbs/candidate/collections/"+table, ""); response.Code == 200 {
				t.Fatal("diagnostic metadata exposed")
			}
			for _, query := range []string{"from: {name: " + table + "}\nlimit: 1\n", "from: {name: table_00}\nwhere: {op: in, left: {field: native_key}, right: {query: {from: {name: " + table + "}}}}\nlimit: 1\n"} {
				if response := selectedRequest(handler, http.MethodPost, "/v1/databases/candidate/dtql", query); response.Code == 200 {
					t.Fatal("diagnostic ordinary/nested query exposed")
				}
			}
		}
	}
	verifyReads()
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	// Reopen after all driver/metadata handles closed, preserving original assets.
	providers, handler, cleanup, err = configuredHandler()
	if err != nil {
		t.Fatal(err)
	}
	verifyReads()
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("temporary entries before=%d after=%d (legacy bounded spools may be retained)", len(before), len(after))
	for _, entry := range after {
		if strings.HasPrefix(entry.Name(), "ovdb-selected-") {
			t.Fatal("image mode made a selected SQLite copy")
		}
	}
	t.Log("16 native / 11 selected retained; schema, ordinary reads, every key, reconnect, denied writes and no full writable copy verified")
}
