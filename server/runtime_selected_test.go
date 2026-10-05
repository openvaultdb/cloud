package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func selectedInventoryFixture(t *testing.T) (string, []runtimeDatabase) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	command := exec.Command("python3", "-c", "from pathlib import Path; import sys; from test_selected_runtime import build_inventory; print(build_inventory(Path(sys.argv[1])))", directory)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("prepare tiny fixtures: %v\n%s", err, output)
	}
	path := filepath.Join(directory, "output", "inventory.json")
	providers, err := loadRuntimeInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, providers
}
func selectedRequest(handler http.Handler, method, path, query string) *httptest.ResponseRecorder {
	var body string
	if query != "" {
		data, _ := json.Marshal(map[string]string{"query": query})
		body = string(data)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if query != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func TestSelectedRuntimeMixedProfilesAndEveryKey(t *testing.T) {
	_, providers := selectedInventoryFixture(t)
	handler, closeDBs, err := newHandlerWithProviders(providers)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDBs()
	keys := []string{"simple", "a/b", "with space", `quote".$#[]`, "Éire_日本"}
	for _, provider := range providers {
		profile := selectedRequest(handler, http.MethodGet, "/v1/databases/"+provider.ID, "")
		if profile.Code != 200 {
			t.Fatal(profile.Body.String())
		}
		if provider.ID == "candidate" && strings.Contains(profile.Body.String(), `"query":true`) {
			t.Fatal("candidate advertises query")
		}
		if provider.ID == "final" && !strings.Contains(profile.Body.String(), `"query":true`) {
			t.Fatal("final does not advertise checked query")
		}
		for _, table := range provider.SmokeRecordsets {
			query := "from: {name: " + table + "}\nlimit: 100\n"
			response := selectedRequest(handler, http.MethodPost, "/v1/databases/"+provider.ID+"/dtql", query)
			if response.Code != 200 {
				t.Fatalf("%s.%s %d %s", provider.ID, table, response.Code, response.Body.String())
			}
			var page northwindQueryPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Records) != len(keys) {
				t.Fatalf("every key missing: %s %d", table, len(page.Records))
			}
			found := map[string]bool{}
			for _, row := range page.Records {
				native, ok := row.Data["native_key"].(string)
				if !ok {
					t.Fatal(row)
				}
				found[native] = true
				record := selectedRequest(handler, http.MethodGet, "/v1/databases/"+provider.ID+"/records/"+table+"/"+url.PathEscape(native), "")
				var read struct {
					Key  string         `json:"key"`
					Data map[string]any `json:"data"`
				}
				err := json.Unmarshal(record.Body.Bytes(), &read)
				if record.Code != 200 || err != nil || row.Key != read.Key || !recordDataMatches(row.Data, read.Data) {
					t.Fatalf("key roundtrip %q: %d %s %v", native, record.Code, record.Body.String(), err)
				}
				if provider.ReadProfile != "" {
					if _, ok := row.Data["id"].(float64); !ok {
						t.Fatal("native integer id overwritten")
					}
				}
			}
			for _, key := range keys {
				if !found[key] {
					t.Fatalf("unproved key %q", key)
				}
			}
			projected := selectedRequest(handler, http.MethodPost, "/v1/databases/"+provider.ID+"/dtql", "from: {name: "+table+"}\ncolumns: [{field: native_key}]\nlimit: 100\n")
			var projection northwindQueryPage
			if err := json.Unmarshal(projected.Body.Bytes(), &projection); err != nil || projected.Code != 200 || len(projection.Records) != len(keys) {
				t.Fatal(projected.Body.String())
			}
			for _, row := range projection.Records {
				if len(row.Data) != 1 || row.Key == "" {
					t.Fatal("projected key identity lost")
				}
			}
		}
	}
	// Five diagnostic tables are present in SQLite but inaccessible by every route family.
	for i := 8; i < 13; i++ {
		table := fmt.Sprintf("table_%02d", i)
		for _, path := range []string{"/ovdb/dbs/candidate/collections/" + table, "/v1/databases/candidate/collections/" + table, "/v1/databases/candidate/records/" + table + "/simple"} {
			response := selectedRequest(handler, http.MethodGet, path, "")
			if response.Code == 200 {
				t.Fatalf("diagnostic leaked via %s: %s", path, response.Body.String())
			}
		}
		for _, query := range []string{"from: {name: " + table + "}\nlimit: 1\n", "from: {name: table_00}\njoins: [{type: inner, from: {name: " + table + "}, on: {op: '==', left: {field: native_key}, right: {field: native_key}}}]\nlimit: 1\n", "from: {name: table_00}\nwhere: {op: in, left: {field: native_key}, right: {query: {from: {name: " + table + "}}}}\nlimit: 1\n"} {
			response := selectedRequest(handler, http.MethodPost, "/v1/databases/candidate/dtql", query)
			if response.Code == 200 {
				t.Fatalf("diagnostic query leaked: %s", response.Body.String())
			}
		}
		if strings.Contains(selectedRequest(handler, http.MethodGet, "/v1/databases/candidate", "").Body.String(), table) {
			t.Fatal("diagnostic in metadata")
		}
	}
	for _, id := range []string{"candidate", "final"} {
		ordered := selectedRequest(handler, http.MethodPost, "/v1/databases/"+id+"/dtql", "from: {name: table_00}\norderBy: [{field: id}]\nlimit: 1\n")
		if ordered.Code != 400 || !strings.Contains(ordered.Body.String(), "ordering_unsupported") {
			t.Fatal("bounded ordering not enforced", ordered.Body.String())
		}
	}
}
func TestSelectedRuntimeOriginalPathReplacementAndCleanup(t *testing.T) {
	_, providers := selectedInventoryFixture(t)
	provider := providers[0]
	originalManifest, readErr := os.ReadFile(provider.Manifest)
	if readErr != nil {
		t.Fatal(readErr)
	}
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "ovdb-selected-*"))
	existing := map[string]bool{}
	for _, path := range before {
		existing[path] = true
	}
	handler, closeDBs, err := newHandlerWithProviders([]runtimeDatabase{provider})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "ovdb-selected-*"))
	var sealed []string
	for _, path := range after {
		if !existing[path] {
			sealed = append(sealed, path)
		}
	}
	if len(sealed) != 1 {
		t.Fatalf("expected one private snapshot, got %v", sealed)
	}
	info, err := os.Stat(sealed[0])
	if err != nil || info.Mode().Perm() != 0o500 {
		t.Fatal("snapshot is not sealed", err)
	}
	for _, path := range []string{provider.Manifest, filepath.Join(filepath.Dir(provider.Manifest), provider.ID+".sqlite")} {
		if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	response := selectedRequest(handler, http.MethodPost, "/v1/databases/"+provider.ID+"/dtql", "from: {name: table_00}\nlimit: 1\n")
	if response.Code != 200 {
		t.Fatal("original path replacement changed mounted bytes", response.Body.String())
	}
	if err := closeDBs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sealed[0]); !os.IsNotExist(err) {
		t.Fatal("snapshot not removed on close", err)
	}
	if _, _, err := newHandlerWithProviders([]runtimeDatabase{provider}); err == nil {
		t.Fatal("replaced manifest admitted on new startup")
	}
	if err := os.WriteFile(provider.Manifest, originalManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newHandlerWithProviders([]runtimeDatabase{provider}); err == nil || !strings.Contains(err.Error(), "serving SQLite snapshot pin mismatch") {
		t.Fatal("replaced SQLite admitted on startup", err)
	}
}
func TestSelectedRuntimeFailClosedLinkage(t *testing.T) {
	path, providers := selectedInventoryFixture(t)
	original := providers[0]
	base := filepath.Dir(path)
	manifestPath := original.Manifest
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"selection", "keymap", "wait", "profile", "flag", "publisherPin", "unknownPin", "nullPin", "duplicatePin", "sourceReplace", "coercion"} {
		t.Run(mutation, func(t *testing.T) {
			provider := original
			data := append([]byte(nil), manifestData...)
			switch mutation {
			case "selection":
				data = []byte(strings.ReplaceAll(string(data), `"table_00":`, `"not_selected":`))
			case "keymap":
				data = []byte(strings.ReplaceAll(string(data), `"table_00": "__ovdb_record_id_1"`, `"table_00": "native_key"`))
			case "coercion":
				data = []byte(strings.Replace(string(data), `"id": {type: integer}`, `"id": {type: string}`, 1))
			case "wait":
				data = []byte(strings.ReplaceAll(string(data), "busy_timeout: 0s", "busy_timeout: 1s"))
			case "profile":
				provider.ReadProfile = "unknown"
			case "flag":
				flag := true
				provider.RequirePublishedQuery = &flag
			case "publisherPin":
				pin := *provider.PublisherManifest
				pin.SHA256 = strings.Repeat("0", 64)
				provider.PublisherManifest = &pin
			case "sourceReplace":
				provider.ServingSHA256 = strings.Repeat("0", 64)
			}
			// Changing key map to another native text field passes schema validation but must
			// still fail all-key uniqueness at mount, even with a recomputed manifest pin.
			if mutation == "keymap" {
				data = []byte(strings.ReplaceAll(string(manifestData), `"table_00": "__ovdb_record_id_1"`, `"table_00": "__ovdb_record_id"`))
			}
			if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			defer os.WriteFile(manifestPath, manifestData, 0o600)
			if !reflect.DeepEqual(data, manifestData) {
				provider.ManifestSHA256 = digest(data)
			}
			inventory := runtimeInventory{Version: 2, Databases: []runtimeDatabase{provider}}
			inventory.Databases[0].Manifest = filepath.Base(manifestPath)
			inventory.Databases[0].License = filepath.Base(provider.License)
			encoded, _ := json.Marshal(inventory)
			if mutation == "unknownPin" {
				encoded = []byte(strings.Replace(string(encoded), `"publisherManifest":{`, `"publisherManifest":{"extra":1,`, 1))
			}
			if mutation == "nullPin" {
				var object map[string]any
				_ = json.Unmarshal(encoded, &object)
				object["databases"].([]any)[0].(map[string]any)["publisherManifest"] = nil
				encoded, _ = json.Marshal(object)
			}
			if mutation == "duplicatePin" {
				encoded = []byte(strings.Replace(string(encoded), `"publisherManifest":{`, `"publisherManifest":{"path":"wrong",`, 1))
			}
			candidatePath := filepath.Join(base, "negative.json")
			if err := os.WriteFile(candidatePath, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadRuntimeInventory(candidatePath)
			if err == nil {
				_, close, err := newHandlerWithProviders(loaded)
				if close != nil {
					_ = close()
				}
				if err == nil {
					t.Fatal("invalid linkage admitted", mutation)
				}
			}
		})
	}
}

func TestSelectedRuntimeScansEveryServingKeyAndCleansFailure(t *testing.T) {
	_, providers := selectedInventoryFixture(t)
	provider := providers[0]
	path := filepath.Join(filepath.Dir(provider.Manifest), provider.ID+".sqlite")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expression := range []string{"'reserved%25'", "'../escape'", "char(1)", "''", "CAST(x'80' AS TEXT)"} {
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`UPDATE table_07 SET __ovdb_record_id_1=` + expression + ` WHERE native_key='simple'`)
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		provider.ServingSHA256, err = sha256File(path)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := filepath.Glob(filepath.Join(os.TempDir(), "ovdb-selected-*"))
		_, close, err := newHandlerWithProviders([]runtimeDatabase{provider})
		if close != nil {
			_ = close()
		}
		if err == nil || !strings.Contains(err.Error(), "invalid transport ID") {
			t.Fatalf("unsafe serving key accepted: %s %v", expression, err)
		}
		after, _ := filepath.Glob(filepath.Join(os.TempDir(), "ovdb-selected-*"))
		if !reflect.DeepEqual(before, after) {
			t.Fatal("failed startup left private snapshot")
		}
	}
}

func TestDescriptorFlagsRequireActualBooleans(t *testing.T) {
	for _, value := range []string{`{"read":true,"write":null,"query":false}`, `{"read":true,"query":false}`, `{"read":true,"write":false,"query":"false"}`} {
		var flags map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &flags); err != nil {
			t.Fatal(err)
		}
		if checkedDescriptorCapabilities(flags, false) {
			t.Fatal("invalid flags accepted", value)
		}
	}
}
