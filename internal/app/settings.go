package app

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gwakko/cex-lab/internal/trading"
)

// Prefix is shared by every configuration key.
const Prefix = "CEXLAB_"

// Configuration keys.
const (
	KeyHTTPAddr         = Prefix + "HTTP_ADDR"
	KeyLogLevel         = Prefix + "LOG_LEVEL"
	KeyLogFormat        = Prefix + "LOG_FORMAT"
	KeyDataDir          = Prefix + "DATA_DIR"
	KeyMarkets          = Prefix + "MARKETS"
	KeyConfirmations    = Prefix + "CONFIRMATIONS"
	KeyEVMRPCURL        = Prefix + "EVM_RPC_URL"
	KeyWatchedAddresses = Prefix + "WATCHED_ADDRESSES"
	KeyMetricsEnabled   = Prefix + "METRICS_ENABLED"
	KeyWSMaxConnections = Prefix + "WS_MAX_CONNECTIONS"
	KeyWSQueueSize      = Prefix + "WS_QUEUE_SIZE"
	KeyWSAllowedOrigins = Prefix + "WS_ALLOWED_ORIGINS"
)

// DefaultMarkets is the demo market: BTC in satoshi, USDT in cents, price per
// 0.001 BTC lot, 10/20 bps maker/taker fees.
const DefaultMarkets = "BTC-USDT:BTC:USDT:1:100000:10:20"

// Settings is the typed process configuration.
type Settings struct {
	HTTPAddr         string
	LogLevel         slog.Level
	LogFormat        string // "json" or "text"
	DataDir          string // "" = in-memory adapters
	Markets          []trading.MarketConfig
	Confirmations    uint64
	EVMRPCURL        string            // "" = no EVM node configured
	WatchedAddresses map[string]string // EVM address -> user
	MetricsEnabled   bool
	WSMaxConnections int
	WSQueueSize      int
	WSAllowedOrigins []string
}

// ValidationError lists every invalid key at once.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration:\n  " + strings.Join(e.Problems, "\n  ")
}

// LoadSettings builds Settings from lookup (for example config.Source.Lookup),
// applying defaults for unset keys and validating everything.
func LoadSettings(lookup func(string) (string, bool)) (Settings, error) {
	p := parser{lookup: lookup}
	s := Settings{
		HTTPAddr:         p.addr(KeyHTTPAddr, "127.0.0.1:0"),
		LogLevel:         p.level(KeyLogLevel, slog.LevelInfo),
		LogFormat:        p.oneOf(KeyLogFormat, "json", "json", "text"),
		DataDir:          p.str(KeyDataDir, ""),
		Markets:          p.markets(KeyMarkets, DefaultMarkets),
		Confirmations:    uint64(p.intRange(KeyConfirmations, 3, 1, 1000)),
		EVMRPCURL:        p.url(KeyEVMRPCURL),
		WatchedAddresses: p.watched(KeyWatchedAddresses),
		MetricsEnabled:   p.boolean(KeyMetricsEnabled, true),
		WSMaxConnections: p.intRange(KeyWSMaxConnections, 1000, 1, 1_000_000),
		WSQueueSize:      p.intRange(KeyWSQueueSize, 256, 1, 1_000_000),
		WSAllowedOrigins: p.list(KeyWSAllowedOrigins),
	}
	if len(p.problems) > 0 {
		return Settings{}, &ValidationError{Problems: p.problems}
	}
	return s, nil
}

// Logger builds the configured slog logger writing to w.
func (s Settings) Logger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: s.LogLevel}
	if s.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// AppConfig turns settings into the composition root's Config.
func (s Settings) AppConfig(logger *slog.Logger) Config {
	return Config{
		DataDir:          s.DataDir,
		Markets:          slices.Clone(s.Markets),
		Logger:           logger,
		DisableMetrics:   !s.MetricsEnabled,
		WSMaxConnections: s.WSMaxConnections,
		WSQueueSize:      s.WSQueueSize,
		WSAllowedOrigins: s.WSAllowedOrigins,
	}
}

// LogSources logs which configuration keys were set and where they came from,
// never their values. origin is typically config.Source.Origin.
func LogSources(log *slog.Logger, keys []string, origin func(string) string) {
	for _, k := range keys {
		log.Info("config key loaded", "key", k, "source", origin(k))
	}
}

type parser struct {
	lookup   func(string) (string, bool)
	problems []string
}

func (p *parser) fail(key, format string, args ...any) {
	p.problems = append(p.problems, key+": "+fmt.Sprintf(format, args...))
}

func (p *parser) str(key, def string) string {
	if v, ok := p.lookup(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func (p *parser) addr(key, def string) string {
	v := p.str(key, def)
	if _, port, err := net.SplitHostPort(v); err != nil {
		p.fail(key, "want host:port, got %q", v)
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		p.fail(key, "invalid port in %q", v)
	}
	return v
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.TrimSpace(v))); err != nil {
		p.fail(key, "want debug, info, warn or error, got %q", v)
		return def
	}
	return l
}

func (p *parser) oneOf(key, def string, allowed ...string) string {
	v := strings.ToLower(p.str(key, def))
	if !slices.Contains(allowed, v) {
		p.fail(key, "want one of %s, got %q", strings.Join(allowed, ", "), v)
	}
	return v
}

func (p *parser) intRange(key string, def, lo, hi int) int {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < lo || n > hi {
		p.fail(key, "want an integer in [%d, %d], got %q", lo, hi, v)
		return def
	}
	return n
}

func (p *parser) boolean(key string, def bool) bool {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		p.fail(key, "want true or false, got %q", v)
		return def
	}
	return b
}

func (p *parser) list(key string) []string {
	var out []string
	for _, item := range strings.Split(p.str(key, ""), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// url accepts http(s) and ws(s) URLs; the value itself is never echoed back
// because RPC URLs often carry API keys.
func (p *parser) url(key string) string {
	v := p.str(key, "")
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "ws", "wss"}, u.Scheme) {
		p.fail(key, "want an http(s):// or ws(s):// URL (value not shown)")
	}
	return v
}

var evmAddressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// watched parses "0xADDR=user,0xADDR=user".
func (p *parser) watched(key string) map[string]string {
	out := map[string]string{}
	for _, item := range p.list(key) {
		addr, user, ok := strings.Cut(item, "=")
		addr, user = strings.TrimSpace(addr), strings.TrimSpace(user)
		switch {
		case !ok || user == "":
			p.fail(key, "want 0xADDRESS=user, got %q", item)
		case !evmAddressRE.MatchString(addr):
			p.fail(key, "invalid EVM address %q", addr)
		case out[addr] != "":
			p.fail(key, "address %s listed twice", addr)
		default:
			out[addr] = user
		}
	}
	return out
}

// markets parses "SYMBOL:BASE:QUOTE:TICK:LOT:MAKER_BPS:TAKER_BPS,...".
func (p *parser) markets(key, def string) []trading.MarketConfig {
	var out []trading.MarketConfig
	seen := map[string]bool{}
	for _, item := range strings.Split(p.str(key, def), ",") {
		item = strings.TrimSpace(item)
		f := strings.Split(item, ":")
		if len(f) != 7 {
			p.fail(key, "want SYMBOL:BASE:QUOTE:TICK:LOT:MAKER_BPS:TAKER_BPS, got %q", item)
			continue
		}
		var nums [4]int64
		var bad []string
		for i, s := range f[3:] {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil || n < 0 || (i < 2 && n == 0) || (i >= 2 && n > 10_000) {
				bad = append(bad, s)
			}
			nums[i] = n
		}
		switch {
		case f[0] == "" || f[1] == "" || f[2] == "" || f[1] == f[2]:
			p.fail(key, "market %q needs a symbol and two distinct assets", item)
		case len(bad) > 0:
			p.fail(key, "market %q: tick and lot must be > 0, fees 0..10000 bps (bad: %s)", f[0], strings.Join(bad, ", "))
		case seen[f[0]]:
			p.fail(key, "market %s listed twice", f[0])
		default:
			seen[f[0]] = true
			out = append(out, trading.MarketConfig{Symbol: f[0], Base: f[1], Quote: f[2], Tick: nums[0], Lot: nums[1], MakerFeeBps: nums[2], TakerFeeBps: nums[3]})
		}
	}
	return out
}
