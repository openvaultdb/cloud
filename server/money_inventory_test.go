package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

const adventureWorksProductMoneyQuery = `from: {name: Production.Product, alias: p}
money: {minorUnitScale: 4, divisionScale: 6, rounding: halfEven}
columns:
  - {aggregate: {function: count, args: [{star: true}]}, as: rows}
  - {aggregate: {function: sum, args: [{field: ListPrice, source: p}]}, as: listPriceTotal}
  - {aggregate: {function: avg, args: [{field: ListPrice, source: p}]}, as: listPriceAverage}
  - {aggregate: {function: sum, args: [{field: StandardCost, source: p}]}, as: standardCostTotal}
`

const adventureWorksPurchaseMoneyQuery = `from: {name: Purchasing.PurchaseOrderDetail, alias: d}
money: {minorUnitScale: 4, divisionScale: 6, rounding: halfEven}
where: {op: '>', left: {field: UnitPrice, source: d}, right: {value: "0"}}
columns:
  - {aggregate: {function: count, args: [{star: true}]}, as: rows}
  - {aggregate: {function: sum, args: [{field: UnitPrice, source: d}]}, as: unitPriceTotal}
  - {aggregate: {function: avg, args: [{field: UnitPrice, source: d}]}, as: unitPriceAverage}
  - {aggregate: {function: sum, args: [{binary: {op: '*', left: {field: OrderQty, source: d}, right: {field: UnitPrice, source: d}}}]}, as: extendedTotal}
having: {op: '>', left: {aggregate: {function: sum, args: [{field: UnitPrice, source: d}]}}, right: {value: "100000"}}
`

const adventureWorksSalesBudgetMoneyQuery = `from: {name: Sales.SalesOrderDetail, alias: d}
money: {minorUnitScale: 6, divisionScale: 6, rounding: halfEven}
columns:
  - {aggregate: {function: sum, args: [{field: LineTotal, source: d}]}, as: total}
`

const adventureWorksHeaderMoneyGroupingQuery = `from: {name: Sales.SalesOrderHeader, alias: h}
money: {minorUnitScale: 4, divisionScale: 6, rounding: halfEven}
groupBy: [{field: SalesOrderID, source: h}]
columns:
  - {field: SalesOrderID, source: h}
  - {aggregate: {function: sum, args: [{field: TotalDue, source: h}]}, as: totalDue}
orderBy: [{field: SalesOrderID, source: h}]
limit: 5
`

const adventureWorksHeaderMoneyGroupingCapQuery = `from: {name: Sales.SalesOrderHeader, alias: h}
money: {minorUnitScale: 4, divisionScale: 6, rounding: halfEven}
groupBy: [{field: SalesOrderID, source: h}]
columns:
  - {field: SalesOrderID, source: h}
  - {aggregate: {function: sum, args: [{field: SubTotal, source: h}]}, as: subTotal}
  - {aggregate: {function: sum, args: [{field: TaxAmt, source: h}]}, as: tax}
  - {aggregate: {function: sum, args: [{field: Freight, source: h}]}, as: freight}
  - {aggregate: {function: sum, args: [{field: TotalDue, source: h}]}, as: totalDue}
orderBy: [{field: SalesOrderID, source: h}]
limit: 5
`

func postAdventureWorksDTQL(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/databases/adventureworks/dtql", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestCloudExactMoneyAdventureWorksAggregates(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks")
	for _, test := range []struct {
		name, query string
		want        map[string]string
		rows        float64
	}{
		{
			name:  "all products exact sum and average",
			query: adventureWorksProductMoneyQuery,
			rows:  504,
			want: map[string]string{
				"listPriceTotal":    "221087.79",
				"listPriceAverage":  "438.66625",
				"standardCostTotal": "130335.8925",
			},
		},
		{
			name:  "purchase details exact product sum and average",
			query: adventureWorksPurchaseMoneyQuery,
			rows:  8845,
			want: map[string]string{
				"unitPriceTotal":   "307301.5176",
				"unitPriceAverage": "34.742964",
				"extendedTotal":    "63791994.838",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := postAdventureWorksDTQL(t, handler, test.query)
			if response.Code != http.StatusOK {
				t.Fatalf("Money query returned %d: %s", response.Code, response.Body.String())
			}
			var result northwindQueryPage
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 1 {
				t.Fatalf("Money query returned %d rows: err=%v body=%s", len(result.Records), err, response.Body.String())
			}
			if got, ok := result.Records[0].Data["rows"].(float64); !ok || got != test.rows {
				t.Fatalf("aggregated source rows=%#v (%T), want %v", result.Records[0].Data["rows"], result.Records[0].Data["rows"], test.rows)
			}
			for field, want := range test.want {
				got, ok := result.Records[0].Data[field].(string)
				if !ok || got != want {
					t.Errorf("%s=%#v (%T), want exact JSON string %q", field, result.Records[0].Data[field], result.Records[0].Data[field], want)
				}
			}
		})
	}
}

func TestCloudExactMoneyKeepsTheSourceRowBudget(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks")
	response := postAdventureWorksDTQL(t, handler, adventureWorksSalesBudgetMoneyQuery)
	var result relationalAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("Money budget response is not JSON: %v: %s", err, response.Body.String())
	}
	if budget := result.Error.Budget; response.Code != http.StatusUnprocessableEntity || result.Error.Code != "query_budget_exceeded" || budget.Name != joinexec.BudgetSourceRows || budget.Limit != int64(cloudQueryLimits().MaxSourceRows) || budget.Route != joinexec.RouteInMemory || len(result.Records) != 0 {
		t.Fatalf("121,317-row Money aggregate got %d %+v with %d partial rows, want 422 query_budget_exceeded at %d rows on the in-memory route", response.Code, result.Error, len(result.Records), cloudQueryLimits().MaxSourceRows)
	}
}

func TestCloudExactMoneyGroupsNearTheSourceRowBudget(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks")
	response := postAdventureWorksDTQL(t, handler, adventureWorksHeaderMoneyGroupingQuery)
	if response.Code != http.StatusOK {
		t.Fatalf("near-budget Money grouping returned %d: %s", response.Code, response.Body.String())
	}
	var result northwindQueryPage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 5 {
		t.Fatalf("near-budget Money grouping returned %d rows: err=%v body=%s", len(result.Records), err, response.Body.String())
	}
	var envelope struct {
		Execution struct {
			Route string `json:"route"`
		} `json:"execution"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Execution.Route != joinexec.RouteInMemory {
		t.Fatalf("near-budget Money grouping route=%q, err=%v; want %q", envelope.Execution.Route, err, joinexec.RouteInMemory)
	}
	want := []struct {
		id       float64
		totalDue string
	}{
		{43659, "23153.2339"},
		{43660, "1457.3288"},
		{43661, "36865.8012"},
		{43662, "32474.9324"},
		{43663, "472.3108"},
	}
	for i, expected := range want {
		row := result.Records[i].Data
		if got, ok := row["SalesOrderID"].(float64); !ok || got != expected.id {
			t.Errorf("group %d SalesOrderID=%#v (%T), want %v", i, row["SalesOrderID"], row["SalesOrderID"], expected.id)
		}
		if got, ok := row["totalDue"].(string); !ok || got != expected.totalDue {
			t.Errorf("group %d totalDue=%#v (%T), want exact string %q", i, row["totalDue"], row["totalDue"], expected.totalDue)
		}
	}
}

func TestCloudExactMoneyGroupingKeepsTheAggregationByteCap(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks")
	response := postAdventureWorksDTQL(t, handler, adventureWorksHeaderMoneyGroupingCapQuery)
	var result relationalAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("over-cap Money grouping response is not JSON: %v: %s", err, response.Body.String())
	}
	if response.Code != http.StatusUnprocessableEntity || result.Error.Code != "query_budget_exceeded" || result.Error.Budget.Name != joinexec.BudgetAggregationBytes || result.Error.Budget.Limit != 64<<20 || result.Error.Budget.Route != joinexec.RouteInMemory || len(result.Records) != 0 {
		t.Fatalf("four-accumulator Money grouping returned %d %+v records=%d, want 422 aggregation_bytes at 64 MiB with no partial results", response.Code, result.Error, len(result.Records))
	}
}

func TestCloudAdventureWorksNullableDecimalStaysNull(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks")
	query := `from: {name: Production.Product, alias: p}
where: {op: '==', left: {field: ProductID, source: p}, right: {value: 1}}
columns: [{field: Weight, source: p}]
limit: 1
`
	response := postAdventureWorksDTQL(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("nullable decimal query returned %d: %s", response.Code, response.Body.String())
	}
	var result northwindQueryPage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 1 {
		t.Fatalf("nullable decimal query returned %d rows: err=%v body=%s", len(result.Records), err, response.Body.String())
	}
	value, present := result.Records[0].Data["Weight"]
	if !present || value != nil {
		t.Fatalf("nullable DECIMAL_TEXT Weight = %#v (present=%v), want JSON null", value, present)
	}
}

func TestCloudAdventureWorksDecimalReadPreservesStoredScale(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks")
	query := `from: {name: Production.Product, alias: p}
where: {op: '==', left: {field: ProductID, source: p}, right: {value: 514}}
columns:
  - {field: ListPrice, source: p}
  - {field: StandardCost, source: p}
limit: 1
`
	response := postAdventureWorksDTQL(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("exact decimal read returned %d: %s", response.Code, response.Body.String())
	}
	var result northwindQueryPage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Records) != 1 {
		t.Fatalf("exact decimal read returned %d rows: err=%v body=%s", len(result.Records), err, response.Body.String())
	}
	for field, want := range map[string]string{"ListPrice": "133.3400", "StandardCost": "98.7700"} {
		got, ok := result.Records[0].Data[field].(string)
		if !ok || got != want {
			t.Errorf("stored decimal %s=%#v (%T), want original JSON string %q", field, result.Records[0].Data[field], result.Records[0].Data[field], want)
		}
	}
}

func TestCloudExactMoneyRejectsInexactClientInput(t *testing.T) {
	handler := cloudTestHandler(t, "adventureworks")
	query := `from: {name: Production.Product, alias: p}
money: {minorUnitScale: 4, divisionScale: 6, rounding: halfEven}
where: {op: '>', left: {field: ListPrice, source: p}, right: {value: 0.125}}
columns: [{aggregate: {function: sum, args: [{field: ListPrice, source: p}]}, as: total}]
`
	response := postAdventureWorksDTQL(t, handler, query)
	var result relationalAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid Money response is not JSON: %v: %s", err, response.Body.String())
	}
	if response.Code != http.StatusBadRequest || result.Error.Code != "invalid_dtql" {
		t.Fatalf("fractional binary float in Money query returned %d with error %#v; want 400 invalid_dtql", response.Code, result.Error)
	}
}
