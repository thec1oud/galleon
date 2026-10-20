package pg

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

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

// String describes the source with the DSN's password hidden, so a config
// report can show it.
func (s Source) String() string {
	return fmt.Sprintf("{dsn:%s max_conns:%d min_conns:%d connect_timeout:%v max_conn_lifetime:%v max_conn_idle_time:%v}",
		redactDSN(s.DSN), s.MaxConns, s.MinConns, s.ConnectTimeout, s.MaxConnLifetime, s.MaxConnIdleTime)
}

var (
	passwordKV = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)
	urlDSN     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
)

// IsURLDSN reports whether dsn is in URL form, scheme://..., whatever the
// scheme, rather than keyword/value form, whose values may hold "://"
// themselves.
func IsURLDSN(dsn string) bool { return urlDSN.MatchString(dsn) }

// redactDSN hides the passwords in either DSN form, valid or not.
func redactDSN(dsn string) string {
	if !IsURLDSN(dsn) {
		return passwordKV.ReplaceAllString(dsn, "${1}xxxxx")
	}
	if u, err := url.Parse(dsn); err == nil {
		// Read as pgx reads it.
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
		u.RawQuery = redactQuery(u.RawQuery)
		return u.String()
	}
	// One url.Parse refuses: everything from the first ':' after the scheme
	// to the last '@' is taken for the password, since it can hold '@', '/'
	// or ':' itself. Where that hides more than the password, too much is
	// hidden rather than too little.
	i := strings.Index(dsn, "://") + len("://")
	head, rest := dsn[:i], dsn[i:]
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		if c := strings.Index(rest[:at], ":"); c >= 0 {
			rest = rest[:c+1] + "xxxxx" + rest[at:]
		}
	}
	if q := strings.Index(rest, "?"); q >= 0 {
		rest = rest[:q+1] + redactQuery(rest[q+1:])
	}
	return head + rest
}

// redactQuery hides the value of each password or sslpassword parameter in
// a raw URL query, matching the names as decoded, and of any parameter
// whose name can't be decoded.
func redactQuery(raw string) string {
	if raw == "" {
		return raw
	}
	params := strings.Split(raw, "&")
	for i, p := range params {
		k, _, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		name, err := url.QueryUnescape(k)
		if err != nil || strings.EqualFold(name, "password") || strings.EqualFold(name, "sslpassword") {
			params[i] = k + "=xxxxx"
		}
	}
	return strings.Join(params, "&")
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
