package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayRegressionDeviceRouteBucketIsolation(t *testing.T) {
	calls := 0
	h := testGateway(t, http.NotFoundHandler(), gatewayRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}))
	for i := 0; i < 60; i++ {
		w := gatewayRequest(t, h, "GET", cloudHost, "/oauth/userinfo", nil)
		if w.Code != 200 {
			t.Fatalf("userinfo%d=%d", i, w.Code)
		}
	}
	w := gatewayRequest(t, h, "POST", cloudHost, "/oauth/token", strings.NewReader("grant_type=device_code"))
	t.Logf("after60userinfo requests token status=%d upstreamCalls=%d", w.Code, calls)
	if w.Code != 200 {
		t.Fatalf("independent token polling bucket consumed by userinfo: %d", w.Code)
	}
}

func TestGatewayRegressionTokenThrottleRetainsOAuthShape(t *testing.T) {
	h := testGateway(t, http.NotFoundHandler(), gatewayRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}))
	var w *httptest.ResponseRecorder
	for i := 0; i < 61; i++ {
		w = gatewayRequest(t, h, "POST", cloudHost, "/oauth/token", strings.NewReader("grant_type=device_code"))
	}
	t.Logf("token throttle status=%d body=%s", w.Code, w.Body.String())
	var body struct {
		Error string `json:"error"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &body)
	if err != nil || body.Error != "slow_down" {
		t.Fatalf("OAuth token throttle no longer parseable as slow_down: %v %s", err, w.Body.String())
	}
}
