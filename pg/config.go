package pg

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
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
	// analytics store, chosen with data.Source(name). They have no
	// replicas: a named source's reads and writes all go to it.
	Named map[string]Source `json:"named"`
	// Replicas are streaming replicas of the primary. Reads that begin a
	// transaction on the primary may run on one, as their read
	// consistency allows (see Consistency); writes never do, and
	// data.Source can't name one. Each has a Name, by default replica_1,
	// replica_2 and so on in order, and an optional Zone.
	Replicas []Source `json:"replicas"`
	// Select is how a read chooses among the replicas that qualify:
	// "least_lag" (LeastLag, the default), "random", "round_robin", or
	// "nearest", which prefers replicas in Zone and takes the least
	// lagging of those.
	Select Selection `json:"select"`
	// Zone is this node's zone, which Nearest compares with each
	// replica's.
	Zone string `json:"zone"`
	// MaxLag takes a replica out of rotation while it is further behind
	// the primary than this; it returns once it has caught up. The
	// default is 2s. MaxLagBytes, when set, also takes out a replica more
	// than that many bytes of WAL behind.
	MaxLag      time.Duration `json:"max_lag"`
	MaxLagBytes int64         `json:"max_lag_bytes"`
	// LagPoll is how often each replica's replay position, health and
	// lag are read, in the background; the default is 50ms. Choosing a
	// replica for a read only compares the latest readings.
	LagPoll time.Duration `json:"lag_poll"`
	// Consistency is the read consistency of reads that neither ask for
	// one nor run in an operation that declares one, as data.ParseLevel
	// reads it. The default is "read_your_writes".
	Consistency string `json:"consistency"`
	// TokenKey signs the causal tokens sent in the Galleon-Consistency
	// response header after a request commits a write, and verifies the
	// ones requests bring back; it must be at least 32 bytes. Without it,
	// read-your-writes holds within a request only. TokenPreviousKey is
	// still accepted, for rotating keys: move the old key there, set a
	// new TokenKey, and drop the old one once tokens signed with it have
	// expired. Nodes sharing a database must share the keys.
	TokenKey         string `json:"token_key" secret:"true"`
	TokenPreviousKey string `json:"token_previous_key" secret:"true"`
	// TokenTTL is how long a token is honored; the default is 10m. An
	// older one is ignored, as a forged or malformed one is.
	TokenTTL time.Duration `json:"token_ttl"`
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
	// Name and Zone apply to replicas only: the name spans, logs and
	// metrics call the replica by, in lower snake case, and the zone it
	// runs in, for the Nearest strategy.
	Name string `json:"name"`
	Zone string `json:"zone"`

	// summary is what String shows of the DSN, set by validation.
	summary string
}

// String describes the source for a config report. It never shows the
// DSN's text, only a summary of what pgx reads from it: user, first host,
// port and database, how many more hosts there are, and a few settings
// that hold no secret (see summarize).
func (s Source) String() string {
	var replica string
	if s.Name != "" || s.Zone != "" {
		replica = fmt.Sprintf(" name:%s zone:%s", s.Name, s.Zone)
	}
	return fmt.Sprintf("{dsn:%s max_conns:%d min_conns:%d connect_timeout:%v max_conn_lifetime:%v max_conn_idle_time:%v%s}",
		s.dsnSummary(), s.MaxConns, s.MinConns, s.ConnectTimeout, s.MaxConnLifetime, s.MaxConnIdleTime, replica)
}

// Selection is how a read chooses among the replicas that qualify for it.
type Selection string

const (
	// LeastLag takes the replica least far behind the primary.
	LeastLag Selection = "least_lag"
	// Random takes one at random.
	Random Selection = "random"
	// RoundRobin takes each in turn.
	RoundRobin Selection = "round_robin"
	// Nearest takes the least lagging of those in the node's Zone, or of
	// all when none is.
	Nearest Selection = "nearest"
)

// Defaults of the replica settings.
const (
	defaultMaxLag   = 2 * time.Second
	defaultLagPoll  = 50 * time.Millisecond
	defaultTokenTTL = 10 * time.Minute
	minLagPoll      = 5 * time.Millisecond
	minTokenKey     = 32
)

var defaultConsistency = data.ReadYourWrites

// invalidDSN stands in for a DSN pgx can't parse, and ambiguousDSN for one
// that may hold a password where pgx reads something else.
const (
	invalidDSN   = "<invalid DSN>"
	ambiguousDSN = "<ambiguous DSN>"
)

// dsnSummary is the summary validation made, or, for a Source never
// validated, one made now.
func (s Source) dsnSummary() string {
	if s.summary != "" {
		return s.summary
	}
	c, err := pgconn.ParseConfig(s.DSN)
	if err != nil {
		return invalidDSN
	}
	return summarize(c, s.DSN)
}

// keywordKey is a key of a keyword/value DSN, or text that looks like one
// inside a quoted value, which only makes the check below stricter.
var keywordKey = regexp.MustCompile(`([A-Za-z_]+)\s*=`)

// maySetSSLPassword reports whether an sslpassword may be set for s, which
// pgx doesn't expose: s has an sslpassword key, or names a service, whose
// file may set one, or the environment sets either. Keys are matched as
// pgx matches them, decoded and case-sensitive; a key that can't be
// decoded counts as a match.
func maySetSSLPassword(s string) bool {
	for _, v := range []string{"PGSSLPASSWORD", "PGSERVICE", "PGSERVICEFILE"} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	risky := func(key string) bool { return key == "sslpassword" || key == "service" || key == "servicefile" }
	if !dsn.IsURL(s) {
		for _, m := range keywordKey.FindAllStringSubmatch(s, -1) {
			if risky(m[1]) {
				return true
			}
		}
		return false
	}
	// Every '?'- or '&'-separated part with an '=', wherever pgx might
	// take the query to start.
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '?' || r == '&' }) {
		k, _, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key, err := url.QueryUnescape(k)
		if err != nil || risky(key) {
			return true
		}
	}
	return false
}

// summarize describes c, parsed from s, as what pgconn.ParseConfig reads
// from it, as
// postgres://user@host:port/database?settings +N hosts. The settings are an
// allowlist: application_name, connect_timeout, and tls, which says how
// TLS is used (off, allowed, preferred, required, verified-ca or verified)
// since pgx keeps sslmode itself only as its effect. An application_name
// is left out if it holds the password, or if an sslpassword may be set at
// all (see maySetSSLPassword). The
// hosts are the others pgx would try.
func summarize(c *pgconn.Config, s string) string {
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
	if v := c.RuntimeParams["application_name"]; v != "" && (c.Password == "" || !strings.Contains(v, c.Password)) && !maySetSSLPassword(s) {
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
	case c.TLSConfig.InsecureSkipVerify && c.TLSConfig.VerifyPeerCertificate != nil:
		// verify-ca, or require with an sslrootcert, which libpq treats
		// as verify-ca: the certificate is checked, the host name isn't.
		params.Set("tls", "verified-ca")
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
	*c = Config{Leaks: leaksError, Select: LeastLag, MaxLag: defaultMaxLag, LagPoll: defaultLagPoll,
		Consistency: defaultConsistency.String(), TokenTTL: defaultTokenTTL}
}

// withDefaults fills in the replica settings left zero, for a Config that
// didn't come from Default.
func (c *Config) withDefaults() {
	if c.Leaks == "" {
		c.Leaks = leaksError
	}
	if c.Select == "" {
		c.Select = LeastLag
	}
	if c.MaxLag == 0 {
		c.MaxLag = defaultMaxLag
	}
	if c.LagPoll == 0 {
		c.LagPoll = defaultLagPoll
	}
	if strings.TrimSpace(c.Consistency) == "" {
		c.Consistency = defaultConsistency.String()
	}
	if c.TokenTTL == 0 {
		c.TokenTTL = defaultTokenTTL
	}
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
	if c.Primary.Name != "" || c.Primary.Zone != "" {
		errs = append(errs, errors.New("primary: name and zone apply to replicas only"))
	}
	names := make([]string, 0, len(c.Named))
	for name := range c.Named {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch {
		case name == data.PrimarySource:
			errs = append(errs, fmt.Errorf("named: %q is the primary's name", name))
		case !sourceName.MatchString(name):
			errs = append(errs, fmt.Errorf("named: source name %q must be lower snake case", name))
		}
		src := c.Named[name]
		if err := src.validate(); err != nil {
			errs = append(errs, fmt.Errorf("named %q: %w", name, err))
		}
		if src.Name != "" || src.Zone != "" {
			errs = append(errs, fmt.Errorf("named %q: name and zone apply to replicas only; a named source has no replicas", name))
		}
		c.Named[name] = src
	}
	errs = append(errs, c.validateReplicas()...)
	if c.RetryAttempts < 0 || c.RetryBackoff < 0 || c.RetryBackoffMax < 0 {
		errs = append(errs, errors.New("retry_attempts, retry_backoff and retry_backoff_max must not be negative"))
	}
	if c.Leaks != leaksError && c.Leaks != leaksLog {
		errs = append(errs, fmt.Errorf("leaks must be %q or %q, not %q", leaksError, leaksLog, c.Leaks))
	}
	return errors.Join(errs...)
}

func (s *Source) validate() error {
	s.summary = "" // whatever an earlier validation found no longer holds
	if strings.TrimSpace(s.DSN) == "" {
		return errors.New(`dsn must be set ("postgres://" takes everything from the PG* environment variables)`)
	}
	if s.MaxConns < 0 || s.MinConns < 0 || (s.MaxConns > 0 && s.MinConns > s.MaxConns) {
		return errors.New("max_conns and min_conns must not be negative, and min_conns must not exceed max_conns")
	}
	if s.ConnectTimeout < 0 || s.MaxConnLifetime < 0 || s.MaxConnIdleTime < 0 {
		return errors.New("timeouts must not be negative")
	}
	pc, err := s.parseDSN()
	if err != nil {
		return err
	}
	s.summary = summarize(&pc.ConnConfig.Config, s.DSN)
	return s.apply(pc)
}

// poolConfig parses the DSN and applies the source's settings.
func (s Source) poolConfig() (*pgxpool.Config, error) {
	pc, err := s.parseDSN()
	if err != nil {
		return nil, err
	}
	return pc, s.apply(pc)
}

func (s Source) parseDSN() (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(s.DSN)
	if err != nil {
		// pgx masks passwords in its message only on a best-effort basis
		// for malformed input, so none of it is repeated.
		return nil, errors.New("dsn can't be parsed: check its syntax")
	}
	return pc, nil
}

// apply applies the source's settings to pc.
func (s Source) apply(pc *pgxpool.Config) error {
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
		return fmt.Errorf("min_conns (%d) exceeds the pool's max_conns (%d)", pc.MinConns, pc.MaxConns)
	}
	return nil
}

func (c *Config) retryPolicy() data.RetryPolicy {
	return data.RetryPolicy{Attempts: c.RetryAttempts, Backoff: c.RetryBackoff, MaxBackoff: c.RetryBackoffMax}
}

// validateReplicas checks the replicas and the settings that go with them,
// naming the replicas that have no name. Zero settings take the defaults.
func (c *Config) validateReplicas() []error {
	var errs []error
	seen := map[string]bool{}
	for i := range c.Replicas {
		r := &c.Replicas[i]
		if r.Name == "" {
			r.Name = fmt.Sprintf("replica_%d", i+1)
		}
		label := fmt.Sprintf("replicas[%d]", i)
		switch {
		case r.Name == data.PrimarySource:
			errs = append(errs, fmt.Errorf("%s: name %q is the primary's", label, r.Name))
		case !sourceName.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s: name %q must be lower snake case", label, r.Name))
		case seen[r.Name]:
			errs = append(errs, fmt.Errorf("%s: name %q is already another replica's", label, r.Name))
		}
		if _, ok := c.Named[r.Name]; ok {
			errs = append(errs, fmt.Errorf("%s: name %q is already a named source's", label, r.Name))
		}
		seen[r.Name] = true
		if err := r.validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s (%s): %w", label, r.Name, err))
		}
	}
	switch c.Select {
	case "", LeastLag, Random, RoundRobin, Nearest:
	default:
		errs = append(errs, fmt.Errorf("select must be %q, %q, %q or %q, not %q", LeastLag, Random, RoundRobin, Nearest, c.Select))
	}
	if c.Select == Nearest && strings.TrimSpace(c.Zone) == "" {
		errs = append(errs, errors.New("select \"nearest\" needs zone, the zone this node runs in"))
	}
	if c.MaxLag < 0 {
		errs = append(errs, errors.New("max_lag must not be negative"))
	}
	if c.MaxLagBytes < 0 {
		errs = append(errs, errors.New("max_lag_bytes must not be negative"))
	}
	if c.LagPoll < 0 || (c.LagPoll > 0 && c.LagPoll < minLagPoll) {
		errs = append(errs, fmt.Errorf("lag_poll must be at least %v", minLagPoll))
	}
	if strings.TrimSpace(c.Consistency) != "" {
		if _, err := data.ParseLevel(c.Consistency); err != nil {
			errs = append(errs, fmt.Errorf("consistency: %w", err))
		}
	}
	// The keys' text is never repeated.
	if c.TokenKey != "" && len(c.TokenKey) < minTokenKey {
		errs = append(errs, fmt.Errorf("token_key must be at least %d bytes", minTokenKey))
	}
	if c.TokenPreviousKey != "" {
		switch {
		case c.TokenKey == "":
			errs = append(errs, errors.New("token_previous_key is only accepted beside a token_key"))
		case len(c.TokenPreviousKey) < minTokenKey:
			errs = append(errs, fmt.Errorf("token_previous_key must be at least %d bytes", minTokenKey))
		}
	}
	if c.TokenTTL < 0 {
		errs = append(errs, errors.New("token_ttl must not be negative"))
	}
	return errs
}

func (c *Config) defaultLevel() data.Level {
	l, err := data.ParseLevel(c.Consistency)
	if err != nil {
		return defaultConsistency
	}
	return l
}
