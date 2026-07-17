package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func TestProxyRemovesUserFromChatCompletions(t *testing.T) {
	var received map[string]any
	handler := testProxyHandler(t, func(r *http.Request) *http.Response {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		return testResponse(http.StatusNoContent, "")
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","user":"cursor","stream":true}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if _, ok := received["user"]; ok {
		t.Fatal("upstream body still contains user")
	}
	if received["model"] != "gpt-test" || received["stream"] != true {
		t.Fatalf("unexpected upstream body: %#v", received)
	}
}

func TestProxyLeavesOtherPathsUnchanged(t *testing.T) {
	const body = `{"user":"keep-me"}`
	var received string
	handler := testProxyHandler(t, func(r *http.Request) *http.Response {
		value, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		received = string(value)
		return testResponse(http.StatusOK, "")
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if received != body {
		t.Fatalf("upstream body = %q, want %q", received, body)
	}
}

func TestProxyTruncatesLongCallIDsInResponses(t *testing.T) {
	const longID = "call_0123456789012345678901234567890123456789012345678901234567890123456789" // 74 chars
	var received map[string]any
	handler := testProxyHandler(t, func(r *http.Request) *http.Response {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		return testResponse(http.StatusOK, "")
	})
	body := `{"model":"m","input":[` +
		`{"type":"function_call","call_id":"` + longID + `","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"` + longID + `","output":"ok"}]}`
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	input, ok := received["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("unexpected input: %#v", received["input"])
	}
	call := input[0].(map[string]any)["call_id"].(string)
	output := input[1].(map[string]any)["call_id"].(string)
	if len(call) != maxCallIDLen {
		t.Fatalf("call_id length = %d, want %d", len(call), maxCallIDLen)
	}
	if call != output {
		t.Fatalf("paired call_ids diverged: %q vs %q", call, output)
	}
	if call != longID[:maxCallIDLen] {
		t.Fatalf("call_id = %q, want prefix of original", call)
	}
}

func TestProxyTruncatesLongToolCallIDsInChatCompletions(t *testing.T) {
	const longID = "call_0123456789012345678901234567890123456789012345678901234567890123456789" // 74 chars
	var received map[string]any
	handler := testProxyHandler(t, func(r *http.Request) *http.Response {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		return testResponse(http.StatusOK, "")
	})
	body := `{"model":"m","messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"` + longID + `","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"` + longID + `","content":"ok"}]}`
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	messages := received["messages"].([]any)
	callID := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
	resultID := messages[1].(map[string]any)["tool_call_id"].(string)
	if len(callID) != maxCallIDLen {
		t.Fatalf("tool_call id length = %d, want %d", len(callID), maxCallIDLen)
	}
	if callID != resultID {
		t.Fatalf("paired ids diverged: %q vs %q", callID, resultID)
	}
	if callID != longID[:maxCallIDLen] {
		t.Fatalf("id = %q, want prefix of original", callID)
	}
}

func TestProxyStripsClientIPHeaders(t *testing.T) {
	var got http.Header
	handler := testProxyHandler(t, func(r *http.Request) *http.Response {
		got = r.Header.Clone()
		return testResponse(http.StatusOK, "")
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	request.RemoteAddr = "203.0.113.7:5555"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	request.Header.Set("Cf-Connecting-Ip", "203.0.113.7")
	request.Header.Set("X-Real-Ip", "203.0.113.7")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	for _, header := range []string{"X-Forwarded-For", "Cf-Connecting-Ip", "X-Real-Ip", "True-Client-Ip", "X-Forwarded-Host", "Forwarded"} {
		if value := got.Get(header); value != "" {
			t.Errorf("upstream received %s=%q, want empty", header, value)
		}
	}
}

func TestProxyRejectsOversizedBody(t *testing.T) {
	upstreamCalled := false
	handler := testProxyHandler(t, func(r *http.Request) *http.Response {
		upstreamCalled = true
		return testResponse(http.StatusOK, "")
	})
	response := httptest.NewRecorder()
	newProxyHandler(handler.proxy, 4, false).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("12345")))

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
	if upstreamCalled {
		t.Fatal("upstream was called")
	}
}

func TestHealthzDoesNotReachUpstream(t *testing.T) {
	upstreamCalled := false
	handler := testProxyHandler(t, func(r *http.Request) *http.Response {
		upstreamCalled = true
		return testResponse(http.StatusOK, "")
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
		t.Fatalf("unexpected health response: status=%d body=%q", response.Code, response.Body.String())
	}
	if upstreamCalled {
		t.Fatal("health check reached upstream")
	}
}

type roundTripFunc func(*http.Request) *http.Response

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r), nil
}

type proxyHandler struct {
	proxy *httputil.ReverseProxy
	http.Handler
}

func testProxyHandler(t *testing.T, roundTrip roundTripFunc) *proxyHandler {
	t.Helper()
	upstreamURL, err := url.Parse("http://upstream.example")
	if err != nil {
		t.Fatal(err)
	}
	proxy := newReverseProxy(upstreamURL)
	proxy.Transport = roundTrip
	return &proxyHandler{proxy: proxy, Handler: newProxyHandler(proxy, 1024, false)}
}

func testResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func TestParseUpstreamURL(t *testing.T) {
	for _, raw := range []string{"", "localhost:8080", "ftp://example.com", "://bad"} {
		if _, err := parseUpstreamURL(raw); err == nil {
			t.Errorf("parseUpstreamURL(%q) unexpectedly succeeded", raw)
		}
	}
	if _, err := parseUpstreamURL("https://example.com/api"); err != nil {
		t.Fatalf("valid URL rejected: %v", err)
	}
}
