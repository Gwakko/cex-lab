package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gwakko/cex-lab/internal/marketdata/adapters/ws"
	"github.com/gwakko/cex-lab/internal/platform/config"
)

func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestSettingsDefaults(t *testing.T) {
	s, err := LoadSettings(lookupMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.HTTPAddr != "127.0.0.1:0" || s.LogLevel != slog.LevelInfo || s.LogFormat != "json" || s.DataDir != "" ||
		s.Confirmations != 3 || s.EVMRPCURL != "" || len(s.WatchedAddresses) != 0 || !s.MetricsEnabled ||
		s.WSMaxConnections != 1000 || s.WSQueueSize != 256 || len(s.WSAllowedOrigins) != 0 {
		t.Fatalf("defaults %+v", s)
	}
	if len(s.Markets) != 1 || s.Markets[0].Symbol != "BTC-USDT" || s.Markets[0].Lot != 100_000 || s.Markets[0].TakerFeeBps != 20 {
		t.Fatalf("default markets %+v", s.Markets)
	}
}

func TestSettingsParsesEveryKey(t *testing.T) {
	s, err := LoadSettings(lookupMap(map[string]string{
		KeyHTTPAddr:         "0.0.0.0:8080",
		KeyLogLevel:         "debug",
		KeyLogFormat:        "TEXT",
		KeyDataDir:          "/var/lib/cexlab",
		KeyMarkets:          "BTC-USDT:BTC:USDT:1:100000:10:20, ETH-USDT:ETH:USDT:5:1:0:50",
		KeyConfirmations:    "12",
		KeyEVMRPCURL:        "http://127.0.0.1:8545",
		KeyWatchedAddresses: "0x000000000000000000000000000000000000A11C=alice, 0x0000000000000000000000000000000000000B0B=bob",
		KeyMetricsEnabled:   "false",
		KeyWSMaxConnections: "50",
		KeyWSQueueSize:      "64",
		KeyWSAllowedOrigins: "example.com, *.example.org",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.HTTPAddr != "0.0.0.0:8080" || s.LogLevel != slog.LevelDebug || s.LogFormat != "text" || s.DataDir != "/var/lib/cexlab" ||
		s.Confirmations != 12 || s.EVMRPCURL != "http://127.0.0.1:8545" || s.MetricsEnabled ||
		s.WSMaxConnections != 50 || s.WSQueueSize != 64 || !slices.Equal(s.WSAllowedOrigins, []string{"example.com", "*.example.org"}) {
		t.Fatalf("settings %+v", s)
	}
	if len(s.Markets) != 2 || s.Markets[1].Symbol != "ETH-USDT" || s.Markets[1].Tick != 5 || s.Markets[1].TakerFeeBps != 50 {
		t.Fatalf("markets %+v", s.Markets)
	}
	if s.WatchedAddresses["0x0000000000000000000000000000000000000B0B"] != "bob" || len(s.WatchedAddresses) != 2 {
		t.Fatalf("watched %v", s.WatchedAddresses)
	}
	cfg := s.AppConfig(nil)
	if !cfg.DisableMetrics || cfg.WSMaxConnections != 50 || cfg.WSQueueSize != 64 || len(cfg.Markets) != 2 {
		t.Fatalf("app config %+v", cfg)
	}
}

func TestSettingsReportsEveryInvalidKeyAtOnce(t *testing.T) {
	secret := "https://user:API-KEY-123@node.example/v1"
	_, err := LoadSettings(lookupMap(map[string]string{
		KeyHTTPAddr:         "localhost",
		KeyLogLevel:         "loud",
		KeyLogFormat:        "xml",
		KeyMarkets:          "BTC-USDT:BTC:BTC:1:1:0:0,ETH:ETH:USDT:0:1:0:99999,broken",
		KeyConfirmations:    "0",
		KeyEVMRPCURL:        "ftp" + strings.TrimPrefix(secret, "https"),
		KeyWatchedAddresses: "0x123=alice,nouser",
		KeyMetricsEnabled:   "maybe",
		KeyWSMaxConnections: "-1",
		KeyWSQueueSize:      "lots",
	}))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %v", err)
	}
	for _, key := range []string{KeyHTTPAddr, KeyLogLevel, KeyLogFormat, KeyMarkets, KeyConfirmations, KeyEVMRPCURL,
		KeyWatchedAddresses, KeyMetricsEnabled, KeyWSMaxConnections, KeyWSQueueSize} {
		if !strings.Contains(err.Error(), key+":") {
			t.Errorf("error does not mention %s:\n%v", key, err)
		}
	}
	if n := strings.Count(err.Error(), KeyMarkets+":"); n != 3 {
		t.Errorf("want 3 market problems, got %d", n)
	}
	if n := strings.Count(err.Error(), KeyWatchedAddresses+":"); n != 2 {
		t.Errorf("want 2 watched-address problems, got %d", n)
	}
	if strings.Contains(err.Error(), "API-KEY-123") {
		t.Fatal("validation error leaks the RPC URL")
	}
}

func TestAppFromDotenvFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", `# shared defaults
CEXLAB_MARKETS=ETH-USDT:ETH:USDT:1:1:0:0
CEXLAB_WS_MAX_CONNECTIONS=10
CEXLAB_METRICS_ENABLED=true
CEXLAB_LOG_LEVEL=info
`)
	write(".env.local", `export CEXLAB_WS_MAX_CONNECTIONS=1 # only one client on my laptop
CEXLAB_METRICS_ENABLED='false'
CEXLAB_LOG_LEVEL="warn"
`)
	src, err := config.Load(dir, []string{"CEXLAB_LOG_LEVEL=error", "CEXLAB_DATA_DIR=" + filepath.Join(dir, "data")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(src.Lookup)
	if err != nil {
		t.Fatal(err)
	}
	if s.LogLevel != slog.LevelError || src.Origin(KeyLogLevel) != config.OriginEnv ||
		s.WSMaxConnections != 1 || src.Origin(KeyWSMaxConnections) != ".env.local" ||
		src.Origin(KeyMarkets) != ".env" || s.MetricsEnabled {
		t.Fatalf("settings %+v", s)
	}
	if err := os.MkdirAll(s.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	a, err := New(s.AppConfig(s.Logger(&logs)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Trading.Book(context.Background(), "ETH-USDT"); err != nil {
		t.Fatalf("market from .env not running: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.DataDir, "ledger.journal")); err != nil {
		t.Fatalf("data dir from the environment not used: %v", err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	if resp, err := http.Get(srv.URL + "/metrics"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics should be disabled by .env.local: %v %v", resp, err)
	}
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, err := ws.Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := ws.Dial(context.Background(), url); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("WS max connections from .env.local not applied: %v", err)
	}

	var sources strings.Builder
	LogSources(slog.New(slog.NewJSONHandler(&sources, nil)), src.Keys(Prefix), src.Origin)
	if !strings.Contains(sources.String(), `"key":"CEXLAB_MARKETS","source":".env"`) ||
		!strings.Contains(sources.String(), `"key":"CEXLAB_LOG_LEVEL","source":"environment"`) {
		t.Fatalf("sources not logged:\n%s", sources.String())
	}
	if strings.Contains(sources.String(), "ETH-USDT") || strings.Contains(sources.String(), "warn") {
		t.Fatal("logged a configuration value")
	}
}

func TestEnvExampleIsValid(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vals, err := config.Parse(f, ".env.example")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{KeyHTTPAddr, KeyLogLevel, KeyLogFormat, KeyDataDir, KeyMarkets, KeyConfirmations, KeyEVMRPCURL,
		KeyWatchedAddresses, KeyMetricsEnabled, KeyWSMaxConnections, KeyWSQueueSize, KeyWSAllowedOrigins}
	for _, k := range keys {
		if _, ok := vals[k]; !ok {
			t.Errorf(".env.example does not document %s", k)
		}
	}
	if len(vals) != len(keys) {
		t.Errorf(".env.example has %d keys, settings know %d", len(vals), len(keys))
	}
	if _, err := LoadSettings(lookupMap(vals)); err != nil {
		t.Fatal(err)
	}
}
