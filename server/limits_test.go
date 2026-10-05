package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

const mebibyte = 1 << 20

// The figures below are the measurements the arithmetic in limits.go rests on.
// They were taken in process (no listener) against openvaultdb-go v0.13.0 and
// the providers pinned in providers.json, as the growth in resident Go memory
// (runtime.MemStats Sys minus HeapReleased, sampled every millisecond after a
// forced collection) while one request ran, including the garbage the collector
// had not yet reclaimed. TestCloudMeasureSingleCollectionReads (in
// limits_measure_test.go) repeats the measurement of the last class on demand.
// Re-measure them when the library is bumped or a provider is added or re-pinned;
// the test TestCloudMeasurementsCoverThePinnedProviders fails when the catalogue
// moves.
const (
	// The server with every provider mounted and no request running: 14 MiB of
	// Go-managed memory, plus the 52.4 MiB Linux binary taken as fully resident,
	// plus 13 MiB for stacks, socket buffers and runtime metadata.
	measuredAtRestBytes = 80 * mebibyte
	// The largest in-memory request: a cross-database grouping that stops at the
	// library's 64 MiB aggregation bound held 79.5 MiB; one that stops at the
	// 40,000-row source budget held 74.4 MiB; a join that stops at the 10,000-row
	// join bound held 34 MiB. The exact Money grouping by SalesOrderID held 78.5 MiB.
	// Rounded up to cover a join plus a grouping in one document.
	measuredInMemoryBytes = 90 * mebibyte
	// The largest database-route request: a join of two photo collections that
	// returns 5.6 MB held 42.5 MiB; scaled to the 8 MiB result bound that is 61 MiB.
	measuredDatabaseRouteBytes = 64 * mebibyte
	// The largest request that no gate counts: the read of one whole collection
	// by the query endpoint (POST or GET /v1/databases/{id}/query), which applies no
	// default row limit and stops at the library's 8 MiB buffer. Measured on every
	// one of all 128 collections in the six pinned fixtures across two runs: the
	// heaviest (Sales.SalesOrderHeaderSalesReason) peaked at 69.0 MiB; other large
	// collections included Person.BusinessEntityAddress (62.7 MiB),
	// Person.EmailAddress (58.2 MiB), Person.Address (58.0 MiB), and
	// Person.PersonPhone (56.1 MiB). Production.TransactionHistory was refused
	// at the existing 8 MiB response bound. The 72 MiB allowance still covers
	// this run; collector timing varies, so repeat it with the opt-in measurement.
	measuredUngatedBytes = 72 * mebibyte
	// The in-memory measurements were taken with this source row budget; a larger
	// budget lets a grouping hold more than they show.
	measuredSourceRows = 40_000
	// The share of the instance memory that must stay free.
	requiredMarginPercent = 15
)

// measuredProviders is the catalogue the measurements were taken on.
var measuredProviders = []string{
	"adventureworks@5028a27189b487d6fd8025fafc1307aada707fd2",
	"chinook@26e852cca00101f53a84ef8ee1f1ae389067f5cf",
	"employees@2069e26e8fdb60bdb16507f75569a579cf3da7cf",
	"northwind@e74726515c3833620b54b7a50d1d273276dd23c1",
	"pubs@6c06c5c7395b03ff1a02c2b1a21485add3e1b65b",
	"sakila@cb9a81a8cbedcd8831737f281f888d5d584fae85",
}

// requestClass is a kind of request an instance may run: how many may run at
// once and the measured peak of one of them.
type requestClass struct {
	slots int
	bytes int64
}

// worstCaseBytes is the memory one instance can hold when the requests it may run
// at once are the heaviest it can be handed: the server at rest, every snapshot
// slot full, and as many requests as the concurrency allows, taken from the
// classes in order of weight, each class as often as its gate lets it in. A class
// that no gate counts (a read of a collection) is limited by the concurrency alone,
// so a request that could take a gated slot but weighs more as an ungated read
// takes the ungated one.
func worstCaseBytes(queries server.QueryLimits, snapshots server.SnapshotLimits, concurrency int) int64 {
	classes := []requestClass{
		{queries.InMemory, measuredInMemoryBytes},
		{queries.Database, measuredDatabaseRouteBytes},
		{concurrency, measuredUngatedBytes},
	}
	slices.SortFunc(classes, func(a, b requestClass) int { return cmp.Compare(b.bytes, a.bytes) })
	total := measuredAtRestBytes + int64(snapshots.Slots)*snapshots.Bytes
	free := concurrency
	for _, class := range classes {
		admitted := min(class.slots, free)
		total += int64(admitted) * class.bytes
		free -= admitted
	}
	return total
}

func readServerFile(t *testing.T, elements ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(elements...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// deployedInstance reads the instance settings from the deploy description.
func deployedInstance(t *testing.T) (memoryBytes int64, concurrency int) {
	t.Helper()
	description := readServerFile(t, "..", ".github", "workflows", "deploy-chinook-cloudrun.yml")
	memory := regexp.MustCompile(`--memory=(\d+)Mi\b`).FindStringSubmatch(description)
	if memory == nil {
		t.Fatal("the deploy description does not set --memory in MiB")
	}
	limit := regexp.MustCompile(`--concurrency=(\d+)\b`).FindStringSubmatch(description)
	if limit == nil {
		t.Fatal("the deploy description does not set --concurrency: the platform default is 80, and the memory arithmetic of limits.go needs the deployed value")
	}
	mebibytes, err := strconv.Atoi(memory[1])
	if err != nil {
		t.Fatal(err)
	}
	concurrency, err = strconv.Atoi(limit[1])
	if err != nil {
		t.Fatal(err)
	}
	return int64(mebibytes) * mebibyte, concurrency
}

func TestCloudLimitsFitTheInstance(t *testing.T) {
	memory, concurrency := deployedInstance(t)
	budget := memory * (100 - requiredMarginPercent) / 100
	queries, snapshots := cloudQueryLimits(), cloudSnapshotLimits()
	if concurrency < queries.InMemory+queries.Database {
		t.Fatalf("concurrency %d is below the %d gated slots", concurrency, queries.InMemory+queries.Database)
	}
	if queries.MaxSourceRows > measuredSourceRows {
		t.Errorf("the source row budget %d is above the %d the in-memory measurements were taken with", queries.MaxSourceRows, measuredSourceRows)
	}
	if snapshots.Bytes > 64*mebibyte {
		t.Errorf("a snapshot of %d MiB is above the 64 MiB the arithmetic assumes", snapshots.Bytes/mebibyte)
	}
	worst := worstCaseBytes(queries, snapshots, concurrency)
	if worst > budget {
		t.Errorf("worst case %d MiB exceeds %d MiB (%d%% of %d MiB kept free)", worst/mebibyte, budget/mebibyte, requiredMarginPercent, memory/mebibyte)
	}

	// Controls: the same check must be able to fail.
	t.Run("one more request per instance does not fit", func(t *testing.T) {
		// The deployed concurrency is the largest that fits, not a number picked
		// below a limit that is further away.
		if got := worstCaseBytes(queries, snapshots, concurrency+1); got <= budget {
			t.Errorf("concurrency %d gives %d MiB, within %d MiB: the deployed concurrency %d is not the largest that fits", concurrency+1, got/mebibyte, budget/mebibyte, concurrency)
		}
	})
	t.Run("the platform default concurrency does not fit", func(t *testing.T) {
		if got := worstCaseBytes(queries, snapshots, 80); got <= budget {
			t.Errorf("concurrency 80 gives %d MiB, within %d MiB: the check cannot fail", got/mebibyte, budget/mebibyte)
		}
	})
	t.Run("the library defaults do not fit", func(t *testing.T) {
		if got := worstCaseBytes(server.DefaultQueryLimits(), server.DefaultSnapshotLimits(), concurrency); got <= budget {
			t.Errorf("library defaults give %d MiB, within %d MiB: the limits would not need to be set", got/mebibyte, budget/mebibyte)
		}
	})
}

func TestCloudWorstCaseTakesTheHeaviestRequests(t *testing.T) {
	queries, snapshots := cloudQueryLimits(), cloudSnapshotLimits()
	atRest := int64(measuredAtRestBytes) + int64(snapshots.Slots)*snapshots.Bytes
	for _, test := range []struct {
		concurrency int
		want        int64
	}{
		{1, measuredInMemoryBytes},
		// The second slot goes to a read that no gate counts (72 MiB), not to the
		// database route (64 MiB): the heavier request takes it.
		{2, measuredInMemoryBytes + measuredUngatedBytes},
		{3, measuredInMemoryBytes + 2*measuredUngatedBytes},
	} {
		if got := worstCaseBytes(queries, snapshots, test.concurrency) - atRest; got != test.want {
			t.Errorf("concurrency %d: requests hold %d MiB, want %d MiB", test.concurrency, got/mebibyte, test.want/mebibyte)
		}
	}
	// A class a gate admits once is counted once, whatever the concurrency.
	if got := worstCaseBytes(server.QueryLimits{InMemory: 1, Database: 1}, snapshots, 100) - atRest; got != measuredInMemoryBytes+99*measuredUngatedBytes {
		t.Errorf("concurrency 100: requests hold %d MiB", got/mebibyte)
	}
}

func TestCloudMeasurementsCoverThePinnedProviders(t *testing.T) {
	var catalogue struct {
		Databases []struct {
			ID       string `json:"id"`
			Revision string `json:"revision"`
		} `json:"databases"`
	}
	if err := json.Unmarshal([]byte(readServerFile(t, "providers.json")), &catalogue); err != nil {
		t.Fatal(err)
	}
	var pinned []string
	for _, database := range catalogue.Databases {
		pinned = append(pinned, database.ID+"@"+database.Revision)
	}
	slices.Sort(pinned)
	if !slices.Equal(pinned, measuredProviders) {
		t.Errorf("the pinned providers are %v; the memory measurements in limits_test.go were taken on %v: re-measure and update both", pinned, measuredProviders)
	}
}

func TestCloudServerOptionsAreAcceptedByTheLibrary(t *testing.T) {
	options := cloudServerOptions()
	if len(options) != 2 {
		t.Fatalf("got %d options, want the query limits and the snapshot limits", len(options))
	}
	// server.New panics on a snapshot limit that is not positive.
	handler := server.New("test", nil, options...).Handler()
	assertCloudQueryDiscovery(t, handler)
}

func getJSON(t *testing.T, handler http.Handler, path string) map[string]any {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, response.Code, response.Body.String())
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("GET %s: %v: %s", path, err, response.Body.String())
	}
	return document
}

// assertCloudQueryDiscovery checks that the discovery document states the limits
// this service enforces and the join engines it allows, no others.
func assertCloudQueryDiscovery(t *testing.T, handler http.Handler) {
	t.Helper()
	query, ok := getJSON(t, handler, "/.well-known/openvaultdb")["query"].(map[string]any)
	if !ok {
		t.Fatal("the discovery document has no query block")
	}
	queries := cloudQueryLimits()
	wantLimits := map[string]float64{
		"timeoutMs":            float64(queries.Timeout / time.Millisecond),
		"maxSourceRows":        float64(queries.MaxSourceRows),
		"maxSourceBytes":       float64(queries.MaxSourceBytes),
		"maxResultRows":        joinexec.MaxResultRows,
		"maxResultBytes":       joinexec.MaxResultBytes,
		"maxInMemoryJoinRows":  joinexec.MaxInMemoryJoinRows,
		"maxInMemoryJoinBytes": joinexec.MaxInMemoryJoinBytes,
		"maxGroups":            joinexec.MaxInMemoryGroups,
	}
	limits, ok := query["limits"].(map[string]any)
	if !ok {
		t.Fatalf("the query block has no limits: %v", query)
	}
	for name, want := range wantLimits {
		if got, ok := limits[name].(float64); !ok || got != want {
			t.Errorf("discovery limit %s = %v, want %v", name, limits[name], want)
		}
	}
	engines, ok := query["joinEngines"].([]any)
	if !ok || len(engines) != 1 || engines[0] != "sqlite" {
		t.Errorf("discovery joinEngines = %v, want [sqlite]", query["joinEngines"])
	}
	if !slices.Equal(queries.JoinEngines, []string{"sqlite"}) {
		t.Errorf("the configured join engines are %v, want [sqlite]: this service mounts SQLite files only", queries.JoinEngines)
	}
}

// cloudTestHandler mounts the named providers from the prepared fixtures, as the
// journey tests do, and skips when they are not prepared. Without names it mounts
// every provider of the inventory.
func cloudTestHandler(t *testing.T, ids ...string) http.Handler {
	t.Helper()
	inventoryPath := os.Getenv("SAMPLE_DATABASES_INVENTORY")
	if inventoryPath == "" {
		t.Skip("set SAMPLE_DATABASES_INVENTORY to generated, checksum-verified serving fixtures")
	}
	all, err := loadRuntimeInventory(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		for _, provider := range all {
			ids = append(ids, provider.ID)
		}
	}
	var providers []runtimeDatabase
	for _, id := range ids {
		index := slices.IndexFunc(all, func(provider runtimeDatabase) bool { return provider.ID == id })
		if index < 0 {
			t.Skipf("provider %s is not in the prepared inventory", id)
		}
		providers = append(providers, all[index])
	}
	handler, closeDatabases, err := newHandlerWithProviders(providers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeDatabases() })
	return handler
}

func TestCloudDiscoveryStatesTheLimitsItEnforces(t *testing.T) {
	handler := cloudTestHandler(t, "chinook", "northwind")
	assertCloudQueryDiscovery(t, handler)
	document := getJSON(t, handler, "/.well-known/openvaultdb")
	databases, _ := document["databases"].([]any)
	if len(databases) != 2 {
		t.Fatalf("discovery lists %d databases, want 2: %v", len(databases), document["databases"])
	}
	for _, entry := range databases {
		database, _ := entry.(map[string]any)
		capabilities, _ := database["capabilities"].(map[string]any)
		id, _ := database["id"].(string)
		// A database that advertises joins is not refused for its engine: the
		// library answers both from one rule.
		if capabilities["joins"] != true || capabilities["aggregation"] != true {
			t.Errorf("database %v does not advertise joins and aggregation: %v", id, capabilities)
		}
		if engine := getJSON(t, handler, "/v1/databases/"+id)["engine"]; engine != "sqlite" {
			t.Errorf("database %v runs on %v, which the join engines do not list", id, engine)
		}
	}
}

type relationalAnswer struct {
	Code      int               `json:"-"`
	Records   []json.RawMessage `json:"records"`
	Execution struct {
		Route string `json:"route"`
	} `json:"execution"`
	Error struct {
		Code   string `json:"code"`
		Budget struct {
			Name  string `json:"name"`
			Limit int64  `json:"limit"`
			Route string `json:"route"`
		} `json:"budget"`
	} `json:"error"`
}

func postRelational(t *testing.T, handler http.Handler, document string) relationalAnswer {
	t.Helper()
	body, err := json.Marshal(map[string]string{"query": document})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/dtql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	answer := relationalAnswer{Code: response.Code}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer is not JSON: %v: %s", err, response.Body.String())
	}
	return answer
}

// A document that reads two databases cannot run as one statement, so the
// server computes it in memory: it is the case the limits are for. The answers
// below are asserted by status, error code and the budget the library names; the
// memory the process uses is not what the test looks at.
func TestCloudRefusesInMemoryQueriesOverTheBudget(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks", "chinook")
	genreJoin := func(collection, localKey, genreKey string) string {
		return fmt.Sprintf("from:\n  database: adventureworks\n  name: '%s'\n  alias: d\n  joins:\n    - type: left\n      from: {database: chinook, name: Genre, alias: g}\n      on:\n        - {left: {field: %s, source: d}, op: '==', right: {field: %s, source: g}}\n", collection, localKey, genreKey)
	}
	grouped := func(collection, localKey, groupField string) string {
		return genreJoin(collection, localKey, "GenreId") +
			fmt.Sprintf("groupBy:\n  - {field: %s, source: d}\ncolumns:\n  - {field: %s, source: d}\n  - {aggregate: {function: count, args: [{star: true}]}, as: n}\nlimit: 5\n", groupField, groupField)
	}
	limits := cloudQueryLimits()

	t.Run("a join that holds more rows than the join bound is refused", func(t *testing.T) {
		document := "from:\n  database: chinook\n  name: Genre\n  alias: g\n  joins:\n    - type: inner\n      from: {database: adventureworks, name: 'Sales.SalesOrderHeader', alias: h}\n      on:\n        - {left: {field: GenreId, source: g}, op: '==', right: {field: SalesPersonID, source: h}}\ncolumns:\n  - {field: id, source: h}\nlimit: 5\n"
		answer := postRelational(t, handler, document)
		if budget := answer.Error.Budget; answer.Code != http.StatusUnprocessableEntity || answer.Error.Code != "query_budget_exceeded" || budget.Name != joinexec.BudgetJoinScan || budget.Route != joinexec.RouteInMemory {
			t.Errorf("got %d %+v, want 422 query_budget_exceeded naming %s on the in-memory route", answer.Code, answer.Error, joinexec.BudgetJoinScan)
		}
	})
	t.Run("a grouping that reads more rows than the source budget is refused", func(t *testing.T) {
		answer := postRelational(t, handler, grouped("Sales.SalesOrderDetail", "ProductID", "id"))
		if budget := answer.Error.Budget; answer.Code != http.StatusUnprocessableEntity || answer.Error.Code != "query_budget_exceeded" || budget.Name != joinexec.BudgetSourceRows || budget.Limit != int64(limits.MaxSourceRows) || budget.Route != joinexec.RouteInMemory {
			t.Errorf("got %d %+v, want 422 query_budget_exceeded naming %s with limit %d on the in-memory route", answer.Code, answer.Error, joinexec.BudgetSourceRows, limits.MaxSourceRows)
		}
	})
	t.Run("a grouping under the source budget answers rows", func(t *testing.T) {
		// 19,972 rows are read: under the budget, and over what one 10,000-row join holds.
		answer := postRelational(t, handler, grouped("Person.Person", "BusinessEntityID", "PersonType"))
		if answer.Code != http.StatusOK || len(answer.Records) != 5 || answer.Execution.Route != joinexec.RouteInMemory {
			t.Errorf("got %d with %d rows on route %q, want 200 with 5 rows on the in-memory route", answer.Code, len(answer.Records), answer.Execution.Route)
		}
	})
	t.Run("a join under the join bound answers rows", func(t *testing.T) {
		document := "from:\n  database: chinook\n  name: Genre\n  alias: g\n  joins:\n    - type: inner\n      from: {database: adventureworks, name: 'Production.Product', alias: p}\n      on:\n        - {left: {field: GenreId, source: g}, op: '==', right: {field: ProductID, source: p}}\ncolumns:\n  - {field: id, source: p}\nlimit: 5\n"
		if answer := postRelational(t, handler, document); answer.Code != http.StatusOK || answer.Execution.Route != joinexec.RouteInMemory {
			t.Errorf("got %d on route %q, want 200 on the in-memory route", answer.Code, answer.Execution.Route)
		}
	})
}

// The query endpoint of one database (POST /v1/databases/{id}/query) is not
// behind the query gate and applies no default row limit: the library reads a
// collection into memory until its answer reaches an 8 MiB buffer, then refuses.
// The arithmetic of limits.go counts it as the heaviest request that no gate
// counts, so this test reads every collection of every mounted database through
// it and checks the answer stays inside that buffer, whatever the library does
// with a larger one. The memory a read holds is not what the test looks at; the
// measurement of it is TestCloudMeasureSingleCollectionReads.
func TestCloudSingleCollectionReadsStayInsideTheLibraryBuffer(t *testing.T) {
	const (
		// The library counts the JSON of the data and the key of every row up to
		// 8 MiB; the answer adds the field names of the envelope.
		answerBound = 9 * mebibyte
		// An answer is "large" when it is within a factor of two of the buffer.
		largeAnswer = 4 * mebibyte
	)
	handler := cloudTestHandler(t)
	var databases []string
	for _, entry := range getJSON(t, handler, "/v1/databases")["databases"].([]any) {
		databases = append(databases, entry.(map[string]any)["id"].(string))
	}
	var largest, answered, refused int
	for _, id := range databases {
		for _, collection := range getJSON(t, handler, "/v1/databases/"+id)["collections"].([]any) {
			code, body := wireRead(t, handler, id, collection.(string))
			switch {
			case code == http.StatusOK:
				answered++
				largest = max(largest, len(body))
				if len(body) > answerBound {
					t.Errorf("%s %s: an unlimited read answered %d MiB, above the %d MiB bound the memory figure rests on", id, collection, len(body)/mebibyte, answerBound/mebibyte)
				}
			case code >= http.StatusBadRequest:
				refused++
				if len(body) > 4096 {
					t.Errorf("%s %s: the refusal %d is %d bytes long", id, collection, code, len(body))
				}
			default:
				t.Errorf("%s %s: unexpected status %d", id, collection, code)
			}
		}
	}
	if answered == 0 {
		t.Fatalf("no collection of %v answered: the test read nothing", databases)
	}

	// Controls: only when AdventureWorks is mounted, whose collections reach the
	// buffer; a smaller local inventory skips them.
	if !slices.Contains(databases, "adventureworks") {
		t.Logf("adventureworks is not mounted: %d answered, %d refused, largest answer %d bytes; controls skipped", answered, refused, largest)
		return
	}
	t.Run("the read reaches the buffer", func(t *testing.T) {
		if largest < largeAnswer {
			t.Errorf("the largest answer is %d bytes: no collection reaches the buffer, so the test cannot show the bound holds", largest)
		}
	})
	t.Run("the buffer refuses what it cannot hold", func(t *testing.T) {
		if code, _ := wireRead(t, handler, "adventureworks", "Production.TransactionHistory"); code < http.StatusBadRequest {
			t.Errorf("a collection of more than 100,000 rows answered %d: the library no longer stops a read at its buffer, so the memory figure no longer bounds it", code)
		}
	})
}

func wireRead(t *testing.T, handler http.Handler, database, collection string) (int, []byte) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, wireRequest(t, database, core.Query{Collection: collection}))
	return response.Code, response.Body.Bytes()
}

func TestPostgresPreviewStaysOff(t *testing.T) {
	// The switch is read by the library when a mount opens: if this service
	// never names it, PostgreSQL queries stay off whatever the environment says.
	needle := core.PreviewPostgresQueriesEnv
	roots := []string{"*.go", "Dockerfile", filepath.Join("..", ".github", "workflows", "*")}
	var files []string
	for _, pattern := range roots {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			if !strings.HasSuffix(match, "_test.go") {
				files = append(files, match)
			}
		}
	}
	if len(files) < 5 {
		t.Fatalf("scanned %v: the deploy description or the server code was not found", files)
	}
	if found := filesNaming(t, needle, files); len(found) > 0 {
		t.Errorf("%s must not be set by this service or its deploy description; it appears in %v", needle, found)
	}

	// Control: the scan must be able to find it.
	planted := filepath.Join(t.TempDir(), "planted.yml")
	if err := os.WriteFile(planted, []byte("env:\n  "+needle+": \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found := filesNaming(t, needle, []string{planted}); len(found) != 1 {
		t.Errorf("the scan did not find a planted %s", needle)
	}
}

func filesNaming(t *testing.T, needle string, files []string) []string {
	t.Helper()
	var found []string
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), needle) {
			found = append(found, file)
		}
	}
	return found
}
