package pg

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/internal/dsn"
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

// String describes the source with the DSN's password hidden, so a config
// report can show it.
func (s Source) String() string {
	return fmt.Sprintf("{dsn:%s max_conns:%d min_conns:%d connect_timeout:%v max_conn_lifetime:%v max_conn_idle_time:%v}",
		redactDSN(s.DSN), s.MaxConns, s.MinConns, s.ConnectTimeout, s.MaxConnLifetime, s.MaxConnIdleTime)
}

var (
	passwordKV = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)
	// queryParam is a query parameter anywhere in a URL DSN, its value
	// running to the next '&'.
	queryParam = regexp.MustCompile(`([?&])([^=&?]*)=([^&]*)`)
)

// redactDSN hides the passwords in either DSN form, valid or not. Since
// parsers disagree on malformed DSNs, it masks in three ways and leans
// towards hiding too much:
//
//   - The userinfo: in a URL, everything after the first ':' that follows
//     "scheme://" and comes before the last '@', whatever lies between, so a
//     password holding '@', '/', '?', '#' or ':' is hidden however a parser
//     splits it. When the last '@' is in the query or the ':' is a port's,
//     more than the password goes.
//   - Parameters: the value of every URL query parameter whose decoded name
//     is password or sslpassword, or can't be decoded, and of every
//     keyword/value password or sslpassword.
//   - What pgx reads: when the DSN carries a password by the above and
//     pgconn.ParseConfig reads one from the DSN itself, every occurrence of
//     that password, raw or escaped, is hidden first. One equal to
//     PGPASSWORD is taken for the environment's and left to the other two
//     masks. (pgx doesn't expose the sslpassword it reads.)
//
// A DSN that carries no password is returned as is.
func redactDSN(s string) string {
	masked := maskDSN(s)
	if masked == s {
		return s
	}
	if pw := dsnPassword(s); pw != "" {
		userinfo := strings.TrimPrefix(url.UserPassword("", pw).String(), ":")
		for _, form := range []string{pw, url.QueryEscape(pw), url.PathEscape(pw), userinfo} {
			s = strings.ReplaceAll(s, form, "xxxxx")
		}
		masked = maskDSN(s)
	}
	return masked
}

// dsnPassword is the password pgx reads from s itself, not from PGPASSWORD
// or a password file.
func dsnPassword(s string) string {
	// A password file is read only when nothing else gives a password, so
	// one that doesn't exist stands in for it. In either form, a later
	// setting wins.
	const nofile = "passfile=/nonexistent/galleon-redaction"
	withNoFile := s + " " + nofile
	if dsn.IsURL(s) {
		withNoFile = s + "?" + nofile
		if strings.Contains(s, "?") {
			withNoFile = s + "&" + nofile
		}
	}
	c, err := pgconn.ParseConfig(withNoFile)
	if err != nil || c.Password == "" || c.Password == os.Getenv("PGPASSWORD") {
		return ""
	}
	return c.Password
}

// maskDSN applies the userinfo and parameter masks.
func maskDSN(s string) string {
	if !dsn.IsURL(s) {
		return passwordKV.ReplaceAllString(s, "${1}xxxxx")
	}
	// Parameters first, so an '@' in a password parameter's value, which
	// the userinfo mask may stop at, is already hidden.
	s = queryParam.ReplaceAllStringFunc(s, func(p string) string {
		m := queryParam.FindStringSubmatch(p)
		name, err := url.QueryUnescape(m[2])
		if err == nil && !strings.EqualFold(name, "password") && !strings.EqualFold(name, "sslpassword") {
			return p
		}
		return m[1] + m[2] + "=xxxxx"
	})
	i := strings.Index(s, "://") + len("://")
	rest := s[i:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return s
	}
	c := strings.Index(rest[:at], ":")
	if c < 0 {
		return s
	}
	return s[:i] + rest[:c+1] + "xxxxx" + rest[at:]
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
