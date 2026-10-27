package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T, name string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

func serveHTTPS(t *testing.T, ln net.Listener, cert tls.Certificate, body string) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})}
	go srv.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
	t.Cleanup(func() { srv.Close() })
}

func pinnedGet(t *testing.T, addr, serverName, fingerprint string) (string, error) {
	t.Helper()
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("tcp", addr)
			},
			TLSClientConfig: &tls.Config{
				ServerName:         serverName,
				InsecureSkipVerify: true,
				VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
					for _, c := range raw {
						sum := sha256.Sum256(c)
						if hex.EncodeToString(sum[:]) == fingerprint {
							return nil
						}
					}
					return errors.New("fingerprint mismatch")
				},
			},
		},
	}
	resp, err := client.Get("https://" + serverName + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestRelayListener(t *testing.T) {
	originCert, originFP := selfSigned(t, "origin")
	originLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveHTTPS(t, originLn, originCert, "origin")
	originHost, originPort, _ := net.SplitHostPort(originLn.Addr().String())

	frontCert, frontFP := selfSigned(t, "front")
	frontTCP, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relayErrs := make(chan error, 16)
	relay := NewRelayListener(frontTCP, "nodes.front.test", originPort, func(id string) (string, bool) {
		if id == "abc123" {
			return originHost, true
		}
		return "", false
	})
	relay.OnError(func(e error) { relayErrs <- e })
	serveHTTPS(t, relay, frontCert, "front")
	frontAddr := frontTCP.Addr().String()

	t.Run("known node is spliced end-to-end with its own certificate", func(t *testing.T) {
		got, err := pinnedGet(t, frontAddr, "abc123.nodes.front.test", originFP)
		if err != nil {
			t.Fatal(err)
		}
		if got != "origin" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("front certificate is never presented for a relayed name", func(t *testing.T) {
		if _, err := pinnedGet(t, frontAddr, "abc123.nodes.front.test", frontFP); err == nil {
			t.Fatal("expected the origin's certificate, got the front's")
		}
	})

	t.Run("other names reach the local server", func(t *testing.T) {
		got, err := pinnedGet(t, frontAddr, "front.test", frontFP)
		if err != nil {
			t.Fatal(err)
		}
		if got != "front" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("no SNI reaches the local server", func(t *testing.T) {
		host, port, _ := net.SplitHostPort(frontAddr)
		got, err := pinnedGet(t, frontAddr, net.JoinHostPort(host, port), frontFP)
		if err != nil {
			t.Fatal(err)
		}
		if got != "front" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("unknown node is refused", func(t *testing.T) {
		_, err := pinnedGet(t, frontAddr, "nope.nodes.front.test", originFP)
		if err == nil {
			t.Fatal("expected a failure")
		}
		select {
		case relayErr := <-relayErrs:
			if !strings.Contains(relayErr.Error(), "unknown node nope") {
				t.Fatalf("relay error = %v", relayErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("relay never reported the unknown node")
		}
	})

	t.Run("concurrent relayed and local clients do not mix", func(t *testing.T) {
		errs := make(chan error, 40)
		for i := 0; i < 20; i++ {
			go func() {
				got, err := pinnedGet(t, frontAddr, "abc123.nodes.front.test", originFP)
				if err == nil && got != "origin" {
					err = errors.New("relayed client got " + got)
				}
				errs <- err
			}()
			go func() {
				got, err := pinnedGet(t, frontAddr, "front.test", frontFP)
				if err == nil && got != "front" {
					err = errors.New("local client got " + got)
				}
				errs <- err
			}()
		}
		for i := 0; i < 40; i++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("non-TLS bytes are dropped", func(t *testing.T) {
		c, err := net.Dial("tcp", frontAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 16)
		if n, err := c.Read(buf); err == nil && n > 0 && buf[0] != 0x15 {
			t.Fatalf("expected close or TLS alert, got %q", buf[:n])
		}
	})
}

func TestRelayListenerNodeID(t *testing.T) {
	rl := &RelayListener{domain: "nodes.front.test"}
	cases := map[string]string{
		"abc.nodes.front.test":  "abc",
		"ABC.Nodes.Front.Test.": "abc",
		"a.b.nodes.front.test":  "",
		"nodes.front.test":      "",
		"abc.nodes.front.tests": "",
		"":                      "",
		"front.test":            "",
	}
	for sni, want := range cases {
		got, ok := rl.nodeID(sni)
		if (want == "") == ok || got != want {
			t.Errorf("nodeID(%q) = %q,%v want %q", sni, got, ok, want)
		}
	}
}

func TestRelayListenerSlowClientIsDropped(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := newRelayListener(tcp, "nodes.front.test", "1", func(string) (string, bool) { return "", false }, 200*time.Millisecond)
	defer relay.Close()

	c, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("silent client must be closed by the relay, got %v", err)
	}
}

func TestRelayListenerStreamsAndHalfCloses(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()
	echoHost, echoPort, _ := net.SplitHostPort(echo.Addr().String())

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := NewRelayListener(tcp, "nodes.front.test", echoPort, func(id string) (string, bool) { return echoHost, id == "n1" })
	defer relay.Close()

	client, err := tls.Dial("tcp", tcp.Addr().String(), &tls.Config{ServerName: "n1.nodes.front.test", InsecureSkipVerify: true})
	if err == nil {
		client.Close()
		t.Fatal("an echo upstream cannot complete a TLS handshake; the relay must have terminated TLS itself")
	}

	raw, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	hello := clientHello(t, "n1.nodes.front.test")
	if _, err := raw.Write(hello); err != nil {
		t.Fatal(err)
	}
	raw.SetDeadline(time.Now().Add(3 * time.Second))
	back := make([]byte, len(hello))
	if _, err := io.ReadFull(raw, back); err != nil || string(back) != string(hello) {
		t.Fatalf("hello was not replayed to the upstream verbatim: %v", err)
	}
	for i := 0; i < 3; i++ {
		msg := []byte(strings.Repeat("x", 4096))
		if _, err := raw.Write(msg); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(raw, make([]byte, len(msg))); err != nil {
			t.Fatalf("stream broke on chunk %d: %v", i, err)
		}
	}
	raw.(*net.TCPConn).CloseWrite()
	if n, err := raw.Read(back); err != io.EOF {
		t.Fatalf("half-close must propagate to EOF, got n=%d err=%v", n, err)
	}
}

func clientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	var out []byte
	sink := &sinkConn{w: func(p []byte) { out = append(out, p...) }}
	tls.Client(sink, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
	if len(out) == 0 {
		t.Fatal("no ClientHello captured")
	}
	return out
}

type sinkConn struct{ w func([]byte) }

func (s *sinkConn) Read(p []byte) (int, error)       { return 0, io.EOF }
func (s *sinkConn) Write(p []byte) (int, error)      { s.w(p); return len(p), nil }
func (s *sinkConn) Close() error                     { return nil }
func (s *sinkConn) LocalAddr() net.Addr              { return nil }
func (s *sinkConn) RemoteAddr() net.Addr             { return nil }
func (s *sinkConn) SetDeadline(time.Time) error      { return nil }
func (s *sinkConn) SetReadDeadline(time.Time) error  { return nil }
func (s *sinkConn) SetWriteDeadline(time.Time) error { return nil }
