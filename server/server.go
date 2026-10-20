// Package server is Galleon's HTTP and RPC transport. It serves every
// operation with an HTTP binding, and every operation with an RPC binding over
// the Connect protocol, gRPC and gRPC-Web (through connect-go), through the
// operation pipeline, on one net/http server whose timeouts and limits are set
// by default. HTTP errors are RFC 9457 problem+json; RPC errors carry the gRPC
// code of their kind.
//
//	galleon.New(..., galleon.Modules(server.Module()))
//
// An HTTP binding's path is a ServeMux pattern, so "GET /" matches every GET
// path the other routes don't; "GET /{$}" matches only the root.
//
// The listener opens once the app is ready (OnReady), so a new generation
// under a supervisor gets no connections before it can serve them, and closes
// when draining begins (OnDrain).
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/codec"
	"github.com/thec1oud/galleon/process"
)

// Config is the "http" config section.
type Config struct {
	// Addr is where to listen: "host:port", ":port", or "unix:/path".
	Addr              string        `json:"addr"`
	ReadHeaderTimeout time.Duration `json:"read_header_timeout"`
	ReadTimeout       time.Duration `json:"read_timeout"`
	WriteTimeout      time.Duration `json:"write_timeout"`
	IdleTimeout       time.Duration `json:"idle_timeout"`
	MaxHeaderBytes    int           `json:"max_header_bytes"`
	// BodyLimit applies to operations that don't set their own.
	BodyLimit int64 `json:"body_limit"`
	// H2C accepts HTTP/2 without TLS, which gRPC clients use inside a cluster.
	H2C bool `json:"h2c"`
	// TLSCert and TLSKey are PEM files. Both or neither must be set.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key" secret:"true"`
	// TrustedProxies are the CIDRs whose X-Forwarded-For header is believed
	// when working out the caller's address. "unix" trusts connections on a
	// Unix socket, from a proxy on the same host. Without it, callers on a
	// Unix socket have no address, so rate limits keyed by address skip them.
	TrustedProxies []string `json:"trusted_proxies"`
	// AccessLog logs one line per request.
	AccessLog bool `json:"access_log"`
}

// Default sets the defaults: timeouts that stop slow clients from holding
// connections, a 1 MiB header limit and a 4 MiB body limit.
func (c *Config) Default() {
	*c = Config{
		Addr:              ":8080",
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BodyLimit:         4 << 20,
		H2C:               true,
	}
}

// Validate checks the section.
func (c *Config) Validate() error {
	var errs []error
	if c.Addr == "" {
		errs = append(errs, errors.New("addr must be set"))
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("tls_cert and tls_key must be set together"))
	}
	if c.BodyLimit <= 0 || c.MaxHeaderBytes <= 0 {
		errs = append(errs, errors.New("body_limit and max_header_bytes must be positive"))
	}
	if c.ReadHeaderTimeout <= 0 || c.ReadTimeout < 0 || c.WriteTimeout < 0 || c.IdleTimeout < 0 {
		errs = append(errs, errors.New("read_header_timeout must be positive, and the other timeouts not negative (zero means none)"))
	}
	if strings.HasPrefix(c.Addr, "unix:") && len(c.Addr) == len("unix:") {
		errs = append(errs, errors.New("addr: unix: needs a path"))
	}
	for _, p := range c.TrustedProxies {
		if p == "unix" {
			continue
		}
		if _, err := netip.ParsePrefix(p); err != nil {
			errs = append(errs, fmt.Errorf("trusted_proxies: %w", err))
		}
	}
	return errors.Join(errs...)
}

// RoleWeb is the role that serves HTTP bindings, and RoleRPC the role that
// serves RPC bindings. A process with both serves both on one listener.
const (
	RoleWeb galleon.Role = "web"
	RoleRPC galleon.Role = "rpc"
)

// Option configures the module, or a server built with New.
type Option func(*options)

type options struct {
	codecs    []codec.Codec
	http, rpc bool
}

// Codecs adds payload formats beyond the built-in JSON. A codec for a media
// type already present replaces it.
func Codecs(c ...codec.Codec) Option { return func(o *options) { o.codecs = append(o.codecs, c...) } }

// Serve chooses which bindings New serves; it serves both by default. Module
// sets it from the process's roles, after any Serve passed to it.
func Serve(http, rpc bool) Option {
	return func(o *options) { o.http, o.rpc = http, rpc }
}

// Module returns the transport module. It serves HTTP bindings in processes
// with the "web" role and RPC bindings (Connect, gRPC and gRPC-Web) in
// processes with the "rpc" role.
func Module(opts ...Option) galleon.Module {
	return galleon.ModuleFunc("http", func(r *galleon.Registry) error {
		cfg := galleon.Config[Config](r, "http")
		web, rpc := r.Active(RoleWeb), r.Active(RoleRPC)
		if !web && !rpc {
			return nil
		}
		opts := append(slices.Clone(opts), Serve(web, rpc))
		var srv *Server
		galleon.Provide[*Server](r, func(ops *galleon.Operations, mw *galleon.HTTPMiddleware, log *slog.Logger, tp trace.TracerProvider) (*Server, error) {
			s, err := New(cfg, ops, mw, log, tp, opts...)
			srv = s
			return s, err
		})
		r.OnReady(func(ctx context.Context) error {
			srv.fatal = r.Fatal
			return srv.Listen(ctx)
		})
		r.OnDrain(func(ctx context.Context) error { return srv.Shutdown(ctx) })
		return nil
	})
}

// Route is one HTTP route and the operation it serves.
type Route struct {
	Method, Path, Operation string
}

// Server serves operations over HTTP.
type Server struct {
	cfg     *Config
	log     *slog.Logger
	tracer  trace.Tracer
	codecs  *codec.Set
	trusted []netip.Prefix
	routes  []Route
	handler http.Handler
	srv     *http.Server

	trustUnix           bool
	fatal               func(error) // stops the app when serving fails after ready
	serveHTTP, serveRPC bool

	mu   sync.Mutex
	ln   net.Listener
	done chan struct{} // closed when Serve returns
}

// New builds the server and its routes. Use Module in an app; New is for
// mounting operations somewhere else.
func New(cfg *Config, ops *galleon.Operations, mw *galleon.HTTPMiddleware, log *slog.Logger, tp trace.TracerProvider, opts ...Option) (*Server, error) {
	o := options{http: true, rpc: true}
	for _, opt := range opts {
		opt(&o)
	}
	s := &Server{
		cfg:       cfg,
		log:       log,
		tracer:    tp.Tracer("github.com/thec1oud/galleon/server"),
		codecs:    codec.NewSet(o.codecs...),
		serveHTTP: o.http,
		serveRPC:  o.rpc,
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	for _, p := range cfg.TrustedProxies {
		if p == "unix" {
			s.trustUnix = true
			continue
		}
		s.trusted = append(s.trusted, netip.MustParsePrefix(p).Masked())
	}
	if mw == nil {
		mw = &galleon.HTTPMiddleware{}
	}
	mux, err := s.routeAll(ops)
	if err != nil {
		return nil, err
	}
	s.handler = s.outer(mw.Wrap(mux))
	s.srv = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		Protocols:         new(http.Protocols),
	}
	s.srv.Protocols.SetHTTP1(true)
	s.srv.Protocols.SetHTTP2(true)
	s.srv.Protocols.SetUnencryptedHTTP2(cfg.H2C)
	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("http: loading the TLS certificate: %w", err)
		}
		s.srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	}
	return s, nil
}

// Handler is the server's whole handler, middleware included, for tests or
// for mounting under another server.
func (s *Server) Handler() http.Handler { return s.handler }

// Routes lists the routes, in registration order.
func (s *Server) Routes() []Route { return s.routes }

// Listen opens the listener and starts serving in the background.
func (s *Server) Listen(ctx context.Context) error {
	if s == nil {
		return errors.New("http: the server was never built")
	}
	network, addr := "tcp", s.cfg.Addr
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		if process.Supervised() {
			// Workers can't share a Unix socket the way they share a port.
			return errors.New("http: a unix: address can't be used under the supervisor; listen on a TCP port")
		}
		network, addr = "unix", p
		if err := removeStaleSocket(addr); err != nil {
			return fmt.Errorf("http: %w", err)
		}
	}
	ln, err := process.Listen(ctx, network, addr)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	if s.srv.TLSConfig != nil {
		ln = tls.NewListener(ln, s.srv.TLSConfig)
	}
	s.mu.Lock()
	s.ln, s.done = ln, make(chan struct{})
	done := s.done
	s.mu.Unlock()
	s.log.InfoContext(ctx, "http server listening", "addr", ln.Addr().String(), "routes", len(s.routes))
	go func() {
		defer close(done)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("http server stopped serving", "error", err)
			if s.fatal != nil {
				s.fatal(fmt.Errorf("http: serving stopped: %w", err))
			}
		}
	}()
	return nil
}

// removeStaleSocket removes a Unix socket file left by a process that is
// gone, which would otherwise stop the listener from binding. A socket that
// still accepts connections is left alone, so the bind fails as it should.
func removeStaleSocket(path string) error {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return nil // nothing there, or not a socket: let the bind report it
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close()
		return fmt.Errorf("%s is in use by another process", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		// A full backlog, a permission problem or a timeout doesn't mean
		// nobody owns the socket, so leave it and let the bind report it.
		return nil
	}
	return os.Remove(path)
}

// Addr is the address the server listens on, once Listen has returned.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Shutdown stops accepting connections and waits for in-flight requests until
// ctx ends, then closes whatever is left.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}

// Stop closes the server if draining didn't, so a provider stop never leaves
// it running.
func (s *Server) Stop(context.Context) error { return s.srv.Close() }
