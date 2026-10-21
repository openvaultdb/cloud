package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/license"
)

// Synthetic data/terms exercise actual Python manifest preparation and Go query
// resolution. They are not publication evidence for the real provider fixtures.
func TestScopedSourceRightsGeneratedManifests(t *testing.T) {
	directory := t.TempDir()
	script := `from pathlib import Path
import sqlite3, sys
import prepare_fixture
root=Path(sys.argv[1])
for db,names in {'northwind':['Customers','Orders'],'chinook':['Customer','Invoice','Album']}.items():
 source=root/(db+'-source.sqlite')
 with sqlite3.connect(source) as connection:
  for name in names:
   connection.execute('CREATE TABLE "'+name+'" (key INTEGER PRIMARY KEY, value TEXT)')
   connection.execute('INSERT INTO "'+name+'" VALUES (1, ?)', ('fixture row',))
 rights={name:{'spdx':'MIT','url':'https://example.test/fixture-terms','text':'Synthetic test terms\n'} for name in names if name not in ('Orders','Album')}
 prepare_fixture.prepare(source,root,db,'','natural',recordset_licenses=rights)
`
	command := exec.Command("python3", "-c", script, directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prepare scoped fixtures: %v\n%s", err, output)
	}
	handler, closeDatabases, err := newHandlerWithManifests(map[string]string{
		"northwind": filepath.Join(directory, "northwind.yaml"),
		"chinook":   filepath.Join(directory, "chinook.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDatabases() })
	for _, item := range []struct {
		database, collection string
		declared             bool
	}{
		{"northwind", "Customers", true}, {"chinook", "Customer", true}, {"chinook", "Invoice", true},
		{"northwind", "Orders", false}, {"chinook", "Album", false},
	} {
		t.Run(item.database+"/"+item.collection, func(t *testing.T) {
			for _, accept := range []string{"application/json", "application/vnd.openvaultdb.query-stream+json"} {
				request := httptest.NewRequest(http.MethodPost, "/v1/databases/"+item.database+"/dtql", strings.NewReader("from: {name: "+item.collection+"}\n"))
				request.Header.Set("Accept", accept)
				request.Host = "untrusted-host.invalid"
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				var result struct {
					Records      []json.RawMessage     `json:"records"`
					Complete     bool                  `json:"complete"`
					SourceRights []license.SourceRight `json:"sourceRights"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || len(result.Records) != 1 || !result.Complete {
					t.Fatalf("%s query: status=%d err=%v body=%s", accept, response.Code, err, response.Body.String())
				}
				if !item.declared {
					if len(result.SourceRights) != 0 {
						t.Fatalf("unselected collection acquired terms: %#v", result.SourceRights)
					}
					continue
				}
				if len(result.SourceRights) != 1 {
					t.Fatalf("expected exactly scoped right: %#v", result.SourceRights)
				}
				right := result.SourceRights[0]
				if right.Source.ServerID != "https://demodb.dev/ovdb" || right.Source.DatabaseID != item.database || right.Source.Recordset != item.collection || right.DeclarationScope != license.RecordsetScope || right.EvidenceOrigin != "server-declared" || right.Declaration.SPDX != "MIT" || right.Declaration.URL != "https://example.test/fixture-terms" || right.Declaration.Text != "Synthetic test terms\n" || len(right.Pins) != 0 {
					t.Fatalf("wrong scope/identity/terms/origin: %#v", right)
				}
			}
		})
	}
}
