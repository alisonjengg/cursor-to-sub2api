package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultMaxBodyBytes int64 = 64 << 20 // 64 MiB
const maxCallIDLen = 64                     // OpenAI-compatible upstreams reject call_id longer than this

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(); err != nil {
			log.Printf("health check failed: %v", err)
			os.Exit(1)
		}
		return
	}

	listenAddr := envString("LISTEN_ADDR", ":8080")
	upstreamRaw := strings.TrimSpace(os.Getenv("UPSTREAM_URL"))
	maxBodyBytes := envInt64("MAX_BODY_BYTES", defaultMaxBodyBytes)
	logRequestBody := envBool("LOG_REQUEST_BODY", false)

	upstream, err := parseUpstreamURL(upstreamRaw)
	if err != nil {
		log.Fatalf("invalid UPSTREAM_URL %q: %v", upstreamRaw, err)
	}

	proxy := newReverseProxy(upstream)
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           newProxyHandler(proxy, maxBodyBytes, logRequestBody),
		ReadHeaderTimeout: 15 * time.Second,
	}

	shutdownSignals := make(chan os.Signal, 1)
	signal.Notify(shutdownSignals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdownSignals
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}()

	log.Printf("sub2api proxy listening on %s, upstream=%s, log_request_body=%t", listenAddr, upstream.Redacted(), logRequestBody)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("proxy server failed: %v", err)
	}
}

func runHealthcheck() error {
	listenAddr := envString("LISTEN_ADDR", ":8080")
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("invalid LISTEN_ADDR %q: %w", listenAddr, err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", response.Status)
	}
	return nil
}

func parseUpstreamURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("environment variable is required")
	}
	upstream, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
		return nil, errors.New("must be an absolute http or https URL")
	}
	return upstream, nil
}

func newProxyHandler(proxy *httputil.ReverseProxy, maxBodyBytes int64, logRequestBody bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}

		start := time.Now()
		requestID := firstNonEmpty(r.Header.Get("X-Request-Id"), r.Header.Get("X-Request-ID"))

		if shouldInspectBody(r) {
			if err := inspectAndRestoreBody(w, r, maxBodyBytes, logRequestBody); err != nil {
				status := http.StatusBadRequest
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					status = http.StatusRequestEntityTooLarge
				}
				log.Printf("request rejected method=%s path=%s request_id=%q error=%v", r.Method, r.URL.Path, requestID, err)
				http.Error(w, http.StatusText(status), status)
				return
			}
		}

		log.Printf("proxy request method=%s path=%s request_id=%q remote=%s", r.Method, r.URL.Path, requestID, clientIP(r))
		proxy.ServeHTTP(w, r)
		log.Printf("proxy completed method=%s path=%s request_id=%q latency_ms=%d", r.Method, r.URL.Path, requestID, time.Since(start).Milliseconds())
	})
}

func shouldInspectBody(r *http.Request) bool {
	return r.Body != nil && methodCanHaveBody(r.Method)
}

func methodCanHaveBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func inspectAndRestoreBody(w http.ResponseWriter, r *http.Request, maxBodyBytes int64, logRequestBody bool) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}

	if isChatCompletionsRequest(r) {
		filteredBody, removed, err := removeTopLevelJSONKeys(body, "user")
		if err != nil {
			log.Printf("chat parameter filter skipped path=%s error=%v", r.URL.Path, err)
		} else if removed {
			log.Printf("chat parameter filter removed_keys=user path=%s original_bytes=%d filtered_bytes=%d", r.URL.Path, len(body), len(filteredBody))
			body = filteredBody
		}
	}

	if isResponsesRequest(r) {
		truncatedBody, count, err := truncateCallIDsInBody(body, maxCallIDLen)
		if err != nil {
			log.Printf("call_id truncation skipped path=%s error=%v", r.URL.Path, err)
		} else if count > 0 {
			log.Printf("call_id truncation path=%s truncated=%d max_len=%d", r.URL.Path, count, maxCallIDLen)
			body = truncatedBody
		}
	}

	if len(body) > 0 {
		logRequestBodyMetadata(r, body)
		if logRequestBody {
			log.Printf("request body method=%s path=%s body=%s", r.Method, r.URL.Path, string(body))
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func isChatCompletionsRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions"
}

func isResponsesRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/responses"
}

// truncateCallIDsInBody shortens every call_id longer than maxLen so upstreams
// that enforce the 64-char limit accept Cursor's Responses API requests. The
// truncation is deterministic, so paired function_call / function_call_output
// ids stay equal after shortening.
func truncateCallIDsInBody(body []byte, maxLen int) ([]byte, int, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, 0, err
	}
	count := truncateCallIDs(payload, maxLen)
	if count == 0 {
		return body, 0, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	return out, count, nil
}

func truncateCallIDs(node any, maxLen int) int {
	count := 0
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			// call_ids are ASCII, so byte slicing is safe.
			if key == "call_id" {
				if s, ok := child.(string); ok && len(s) > maxLen {
					value[key] = s[:maxLen]
					count++
					continue
				}
			}
			count += truncateCallIDs(child, maxLen)
		}
	case []any:
		for _, child := range value {
			count += truncateCallIDs(child, maxLen)
		}
	}
	return count
}

func removeTopLevelJSONKeys(body []byte, keys ...string) ([]byte, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, err
	}

	removed := false
	for _, key := range keys {
		if _, ok := payload[key]; ok {
			delete(payload, key)
			removed = true
		}
	}
	if !removed {
		return body, false, nil
	}

	filteredBody, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return filteredBody, true, nil
}

func logRequestBodyMetadata(r *http.Request, body []byte) {
	var payload struct {
		Model  string `json:"model"`
		Stream *bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("request body metadata method=%s path=%s body_bytes=%d json_valid=false", r.Method, r.URL.Path, len(body))
		return
	}

	stream := "unset"
	if payload.Stream != nil {
		stream = strconv.FormatBool(*payload.Stream)
	}
	log.Printf("request body metadata method=%s path=%s model=%q stream=%s body_bytes=%d json_valid=true", r.Method, r.URL.Path, payload.Model, stream, len(body))
}

func newReverseProxy(upstream *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalHost := r.Host
		originalDirector(r)
		r.Host = upstream.Host
		if originalHost != "" {
			r.Header.Set("X-Forwarded-Host", originalHost)
		}
		r.Header.Set("X-Forwarded-Proto", forwardedProto(r))
	}
	proxy.FlushInterval = -1
	proxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error method=%s path=%s error=%v", r.Method, r.URL.Path, err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	return proxy
}

func forwardedProto(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func clientIP(r *http.Request) string {
	if value := r.Header.Get("X-Forwarded-For"); value != "" {
		parts := strings.Split(value, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		log.Printf("invalid %s=%q, using default %d", key, value, fallback)
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		log.Printf("invalid %s=%q, using default %t", key, value, fallback)
		return fallback
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
