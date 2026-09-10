package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(cfg *serveConfig, upstreamOverride string) *server {
	return newServer(cfg, newFakeClient(), upstreamOverride, true, time.Millisecond)
}

func TestKubeswap_HandlerNotReady(t *testing.T) {
	s := newTestServer(testConfig(), "")
	rr := httptest.NewRecorder()
	s.handler(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not-ready") {
		t.Errorf("body: %s", rr.Body.String())
	}

	// Non-health paths get the same answer.
	rr2 := httptest.NewRecorder()
	s.handler(rr2, httptest.NewRequest(http.MethodPost, "/completion", nil))
	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for /completion, got %d", rr2.Code)
	}
}

func TestKubeswap_HandlerReadyButNoUpstream(t *testing.T) {
	s := newTestServer(testConfig(), "")
	s.ready.Store(true)
	s.readyReason.Store("pod ready")
	// upstream left empty
	rr := httptest.NewRecorder()
	s.handler(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected 502 with no upstream, got %d", rr.Code)
	}
}

func TestKubeswap_HandlerProxies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	s := newTestServer(testConfig(), "")
	s.upstream.Store(upstream.URL)
	s.ready.Store(true)
	s.readyReason.Store("pod ready")

	rr := httptest.NewRecorder()
	s.handler(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "ok" {
		t.Errorf("body: %s", rr.Body.String())
	}
}

func TestKubeswap_HandlerProxiesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: hello\n\n")
	}))
	defer upstream.Close()

	s := newTestServer(testConfig(), "")
	s.upstream.Store(upstream.URL)
	s.ready.Store(true)
	s.readyReason.Store("pod ready")

	rr := httptest.NewRecorder()
	s.handler(rr, httptest.NewRequest(http.MethodGet, "/completion", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	if !strings.Contains(rr.Body.String(), "data: hello") {
		t.Errorf("body: %s", rr.Body.String())
	}
}

func TestKubeswap_HandlerOverrideUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "from-override")
	}))
	defer upstream.Close()

	s := newTestServer(testConfig(), upstream.URL)
	if s.upstream.Load().(string) != upstream.URL {
		t.Fatalf("override not applied: %v", s.upstream.Load())
	}
	s.ready.Store(true)
	rr := httptest.NewRecorder()
	s.handler(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Body.String() != "from-override" {
		t.Errorf("body: %s", rr.Body.String())
	}
}
