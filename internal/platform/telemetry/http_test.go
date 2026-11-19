package telemetry

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, srv *httptest.Server, path string) (int, string, string) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

func TestHTTPEndpoints(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("trades_total", "Trades.", "market").Inc("BTC-USDT")
	health := &Health{}
	var invariantErr error
	health.AddReadinessCheck("invariants", func() error { return invariantErr })
	health.AddReadinessCheck("journal", func() error { return nil })
	srv := httptest.NewServer(NewMux(reg, health))
	defer srv.Close()

	code, ctype, body := get(t, srv, "/metrics")
	if code != 200 || ctype != ContentType {
		t.Fatalf("/metrics: %d %q", code, ctype)
	}
	if e := parse(t, body); e.samples[`trades_total{market="BTC-USDT"}`] != 1 {
		t.Fatalf("/metrics body:\n%s", body)
	}

	if code, _, _ := get(t, srv, "/healthz"); code != 200 {
		t.Fatalf("/healthz: %d", code)
	}
	if code, _, _ := get(t, srv, "/readyz"); code != 200 {
		t.Fatalf("/readyz healthy: %d", code)
	}

	invariantErr = errors.New("locked != reserves")
	code, _, body = get(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "invariants: locked != reserves") {
		t.Fatalf("/readyz failing: %d %q", code, body)
	}
	if code, _, _ := get(t, srv, "/healthz"); code != 200 {
		t.Fatal("liveness must not depend on readiness checks")
	}
}
