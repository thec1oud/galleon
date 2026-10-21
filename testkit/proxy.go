package testkit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thec1oud/galleon/internal/dsn"
)

// Proxy is a TCP proxy in front of a Postgres server, for tests that need
// to cut a database off and bring it back, or to see what a client sends.
// It reads the frontend side of the protocol as it passes, so it can record
// each message a client sends; it never changes a byte.
type Proxy struct {
	t      testing.TB
	ln     net.Listener
	target string

	mu     sync.Mutex
	conns  map[net.Conn]bool
	cut    bool
	msgs   []Message
	opened int
	wg     sync.WaitGroup
}

// Message is one message a client sent through a Proxy after its startup.
type Message struct {
	// Conn numbers the client connection, from 1 in the order they opened.
	Conn int
	// Type is the message's type byte, such as 'Q' for a simple query,
	// 'P' for Parse or 'S' for Sync.
	Type byte
	// SQL is a simple query's text.
	SQL string
}

// NewProxy listens on a local port and forwards to the first host of dsn,
// until t ends. Its DSN method points a DSN at it.
func NewProxy(t testing.TB, dsn string) *Proxy {
	t.Helper()
	target, err := hostPort(dsn)
	if err != nil {
		t.Fatalf("testkit.NewProxy: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testkit.NewProxy: %v", err)
	}
	p := &Proxy{t: t, ln: ln, target: target, conns: map[net.Conn]bool{}}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		p.Cut()
		p.wg.Wait()
	})
	return p
}

// Addr is where the proxy listens, as host:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// DSN returns dsn with its host and port replaced by the proxy's.
func (p *Proxy) DSN(s string) string {
	p.t.Helper()
	host, port, _ := net.SplitHostPort(p.Addr())
	if !dsn.IsURL(s) {
		return s + " host=" + host + " port=" + port
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		p.t.Fatalf("testkit.Proxy: %v", err)
	}
	u.Host = net.JoinHostPort(host, port)
	q := u.Query()
	q.Del("host")
	q.Del("port")
	u.RawQuery = q.Encode()
	return u.String()
}

// Cut closes every connection through the proxy, and refuses new ones until
// Restore.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
	for c := range p.conns {
		_ = c.Close()
	}
}

// Restore lets connections through again.
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = false
}

// Messages returns what clients sent since the proxy started, or since
// Reset.
func (p *Proxy) Messages() []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Message(nil), p.msgs...)
}

// Opened is how many connections clients have opened to the proxy since it
// started, those it refused while cut and those closed since included.
func (p *Proxy) Opened() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opened
}

// Reset forgets the messages recorded so far.
func (p *Proxy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = nil
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.opened++
		n := p.opened
		refuse := p.cut
		if !refuse {
			p.conns[client] = true
		}
		p.mu.Unlock()
		if refuse {
			_ = client.Close()
			continue
		}
		p.wg.Add(1)
		go p.serve(n, client)
	}
}

func (p *Proxy) serve(id int, client net.Conn) {
	defer p.wg.Done()
	defer p.drop(client)
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.cut {
		p.mu.Unlock()
		_ = server.Close()
		return
	}
	p.conns[server] = true
	p.mu.Unlock()
	defer p.drop(server)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
		close(done)
	}()
	_ = p.frontend(id, client, server)
	_ = server.Close()
	<-done
}

func (p *Proxy) drop(c net.Conn) {
	_ = c.Close()
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

// Startup request codes that are answered with one byte, after which the
// client sends another untyped message.
const (
	sslRequest    = 80877103
	gssencRequest = 80877104
)

// frontend forwards what the client sends, recording each message after
// the startup.
func (p *Proxy) frontend(id int, client, server net.Conn) error {
	head := make([]byte, 8)
	for {
		// Untyped: length, then the protocol version or a request code.
		if _, err := io.ReadFull(client, head); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(head))
		if n < 8 || n > 1<<20 {
			return fmt.Errorf("startup message of %d bytes", n)
		}
		rest := make([]byte, n-8)
		if _, err := io.ReadFull(client, rest); err != nil {
			return err
		}
		if _, err := server.Write(append(head[:8:8], rest...)); err != nil {
			return err
		}
		if code := binary.BigEndian.Uint32(head[4:]); code != sslRequest && code != gssencRequest {
			break
		}
	}
	typed := make([]byte, 5)
	for {
		if _, err := io.ReadFull(client, typed); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(typed[1:]))
		if n < 4 {
			return errors.New("malformed message")
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(client, body); err != nil {
			return err
		}
		m := Message{Conn: id, Type: typed[0]}
		if m.Type == 'Q' {
			m.SQL = strings.TrimSuffix(string(body), "\x00")
		}
		p.mu.Lock()
		p.msgs = append(p.msgs, m)
		p.mu.Unlock()
		if _, err := server.Write(append(typed[:5:5], body...)); err != nil {
			return err
		}
	}
}

// hostPort is a DSN's first host and port.
func hostPort(s string) (string, error) {
	if dsn.IsURL(s) {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil {
			return "", err
		}
		port := u.Port()
		if port == "" {
			port = "5432"
		}
		return net.JoinHostPort(u.Hostname(), port), nil
	}
	host, port := "localhost", "5432"
	for _, f := range strings.Fields(s) {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "host":
			host = strings.Trim(v, "'")
		case "port":
			port = strings.Trim(v, "'")
		}
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("port %q", port)
	}
	return net.JoinHostPort(host, port), nil
}
