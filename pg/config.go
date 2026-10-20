package pg

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/data"
)

// Config is the "pg" config section.
type Config struct {
	// Primary is the database writes go to, and reads unless they name a
	// source.
	Primary Source `json:"primary"`
	// Named are further databases, such as one a module owns or an
	// analytics store, chosen with data.Source(name).
	Named map[string]Source `json:"named"`
	// RetryAttempts, RetryBackoff and RetryBackoffMax are the default retry
	// policy of Read and Write closures (see data.RetryPolicy); zero takes
	// that policy's defaults.
	RetryAttempts   int           `json:"retry_attempts"`
	RetryBackoff    time.Duration `json:"retry_backoff"`
	RetryBackoffMax time.Duration `json:"retry_backoff_max"`
	// Leaks says what happens to a root transaction still open when the
	// operation that began it ends. It is always rolled back. With "error",
	// the default, the call also fails with an Internal error whose cause
	// has the stack of where the transaction began, and it is logged; with
	// "log" it is only logged, and the call's own result stands. Use "error"
	// in development and tests; "log" suits production, where a leaked read
	// shouldn't fail a request that otherwise worked.
	Leaks string `json:"leaks"`
}

// Source is one database.
type Source struct {
	// DSN is a postgres:// URL or a keyword/value string. "postgres://"
	// alone takes everything from the PG* environment variables, as libpq
	// does. It is required.
	DSN string `json:"dsn"`
	// MaxConns and MinConns size the pool; zero takes pgx's defaults (at
	// most the greater of 4 and the number of CPUs, at least none), or
	// pool_max_conns and pool_min_conns in the DSN.
	MaxConns int32 `json:"max_conns"`
	MinConns int32 `json:"min_conns"`
	// ConnectTimeout bounds opening one connection; zero takes the DSN's
	// connect_timeout, or none.
	ConnectTimeout time.Duration `json:"connect_timeout"`
	// MaxConnLifetime and MaxConnIdleTime retire connections; zero takes
	// pgx's defaults of an hour and 30 minutes.
	MaxConnLifetime time.Duration `json:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `json:"max_conn_idle_time"`
}

// String describes the source for a config report. It never shows the
// DSN's text, only a summary of what pgx reads from it: user, first host,
// port and database, how many more hosts there are, and a few settings
// that hold no secret (see summarize).
func (s Source) String() string {
	return fmt.Sprintf("{dsn:%s max_conns:%d min_conns:%d connect_timeout:%v max_conn_lifetime:%v max_conn_idle_time:%v}",
		summary(s.DSN), s.MaxConns, s.MinConns, s.ConnectTimeout, s.MaxConnLifetime, s.MaxConnIdleTime)
}

// invalidDSN stands in for a DSN pgx can't parse, and ambiguousDSN for one
// that may hold a password where pgx reads something else.
const (
	invalidDSN   = "<invalid DSN>"
	ambiguousDSN = "<ambiguous DSN>"
)

// summaries caches summarize's result by a hash of the DSN, so parsing,
// which may read files such as a password file, happens once per DSN.
var summaries sync.Map // [sha256.Size]byte -> string

func summary(s string) string {
	key := sha256.Sum256([]byte(s))
	if v, ok := summaries.Load(key); ok {
		return v.(string)
	}
	v, _ := summaries.LoadOrStore(key, summarize(s))
	return v.(string)
}

// summarize describes s from what pgconn.ParseConfig reads from it, as
// postgres://user@host:port/database?settings +N hosts. The settings are an
// allowlist: application_name, connect_timeout, and tls, which says how TLS is used (off, allowed,
// preferred, required or verified) since pgx keeps sslmode itself only as
// its effect. A setting whose value holds the password is left out. The
// hosts are the others pgx would try.
func summarize(s string) string {
	c, err := pgconn.ParseConfig(s)
	if err != nil {
		return invalidDSN
	}
	// An '@' in what pgx reads as the user, host or database is most
	// likely a password with an unescaped '/' or '@' that the URL split
	// elsewhere, as in postgres://app:5432/secret@db/orders.
	if strings.Contains(c.User+c.Host+c.Database, "@") {
		return ambiguousDSN
	}
	hostPort := func(host string, port uint16) string {
		if strings.HasPrefix(host, "/") { // a Unix socket's directory
			return host + ":" + strconv.Itoa(int(port))
		}
		return net.JoinHostPort(host, strconv.Itoa(int(port)))
	}
	var b strings.Builder
	b.WriteString("postgres://" + url.PathEscape(c.User) + "@" + hostPort(c.Host, c.Port) + "/" + url.PathEscape(c.Database))
	// pgx lists a host twice when it may try it with TLS and without.
	hosts := map[string]bool{hostPort(c.Host, c.Port): true}
	tlsFirst, plainLater, tlsLater := c.TLSConfig != nil, false, false
	for _, f := range c.Fallbacks {
		hosts[hostPort(f.Host, f.Port)] = true
		if f.Host == c.Host && f.Port == c.Port {
			plainLater = plainLater || f.TLSConfig == nil
			tlsLater = tlsLater || f.TLSConfig != nil
		}
	}
	params := url.Values{}
	if v := c.RuntimeParams["application_name"]; v != "" && (c.Password == "" || !strings.Contains(v, c.Password)) {
		params.Set("application_name", v)
	}
	if c.ConnectTimeout > 0 {
		params.Set("connect_timeout", c.ConnectTimeout.String())
	}
	switch {
	case !tlsFirst && !tlsLater:
		params.Set("tls", "off")
	case !tlsFirst:
		params.Set("tls", "allowed")
	case plainLater:
		params.Set("tls", "preferred")
	case c.TLSConfig.InsecureSkipVerify:
		params.Set("tls", "required")
	default:
		params.Set("tls", "verified")
	}
	b.WriteString("?" + params.Encode())
	switch n := len(hosts) - 1; {
	case n == 1:
		b.WriteString(" +1 host")
	case n > 1:
		fmt.Fprintf(&b, " +%d hosts", n)
	}
	return b.String()
}

// Default sets the defaults: no sources, the default retry policy, and
// leaks reported as errors.
func (c *Config) Default() {
	*c = Config{Leaks: leaksError}
}

const (
	leaksError = "error"
	leaksLog   = "log"
)

var sourceName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Validate checks the section, parsing every DSN. Error messages never
// repeat a DSN, which may hold a password.
func (c *Config) Validate() error {
	var errs []error
	if err := c.Primary.validate(); err != nil {
		errs = append(errs, fmt.Errorf("primary: %w", err))
	}
	names := make([]string, 0, len(c.Named))
	for name := range c.Named {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch {
		case name == data.Primary:
			errs = append(errs, fmt.Errorf("named: %q is the primary's name", name))
		case !sourceName.MatchString(name):
			errs = append(errs, fmt.Errorf("named: source name %q must be lower snake case", name))
		}
		if err := c.Named[name].validate(); err != nil {
			errs = append(errs, fmt.Errorf("named %q: %w", name, err))
		}
	}
	if c.RetryAttempts < 0 || c.RetryBackoff < 0 || c.RetryBackoffMax < 0 {
		errs = append(errs, errors.New("retry_attempts, retry_backoff and retry_backoff_max must not be negative"))
	}
	if c.Leaks != leaksError && c.Leaks != leaksLog {
		errs = append(errs, fmt.Errorf("leaks must be %q or %q, not %q", leaksError, leaksLog, c.Leaks))
	}
	return errors.Join(errs...)
}

func (s Source) validate() error {
	if strings.TrimSpace(s.DSN) == "" {
		return errors.New(`dsn must be set ("postgres://" takes everything from the PG* environment variables)`)
	}
	if s.MaxConns < 0 || s.MinConns < 0 || (s.MaxConns > 0 && s.MinConns > s.MaxConns) {
		return errors.New("max_conns and min_conns must not be negative, and min_conns must not exceed max_conns")
	}
	if s.ConnectTimeout < 0 || s.MaxConnLifetime < 0 || s.MaxConnIdleTime < 0 {
		return errors.New("timeouts must not be negative")
	}
	if _, err := s.poolConfig(); err != nil {
		return err
	}
	summary(s.DSN) // parsed now, not when the config is first reported
	return nil
}

// poolConfig parses the DSN and applies the source's settings.
func (s Source) poolConfig() (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(s.DSN)
	if err != nil {
		// pgx masks passwords in its message only on a best-effort basis
		// for malformed input, so none of it is repeated.
		return nil, errors.New("dsn can't be parsed: check its syntax")
	}
	if s.MaxConns > 0 {
		pc.MaxConns = s.MaxConns
	}
	if s.MinConns > 0 {
		pc.MinConns = s.MinConns
	}
	if s.ConnectTimeout > 0 {
		pc.ConnConfig.ConnectTimeout = s.ConnectTimeout
	}
	if s.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = s.MaxConnLifetime
	}
	if s.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = s.MaxConnIdleTime
	}
	if pc.MinConns > pc.MaxConns {
		return nil, fmt.Errorf("min_conns (%d) exceeds the pool's max_conns (%d)", pc.MinConns, pc.MaxConns)
	}
	return pc, nil
}

func (c *Config) retryPolicy() data.RetryPolicy {
	return data.RetryPolicy{Attempts: c.RetryAttempts, Backoff: c.RetryBackoff, MaxBackoff: c.RetryBackoffMax}
}
