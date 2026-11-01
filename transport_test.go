package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCLIHandshakeAndCertificateVerification(t *testing.T) {
	t.Parallel()

	hellos := make(chan *tls.ClientHelloInfo, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached an untrusted TLS server")
	}))
	server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos <- hello
		return nil, nil
	}}

	server.StartTLS()
	defer server.Close()

	transport := newCLITransport()
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: time.Second}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()

		t.Fatal("accepted untrusted server certificate")
	}

	select {
	case hello := <-hellos:
		// These distinguish the observed Bun CLI handshake from standard Go and
		// Chrome profiles. Certificate verification must still run afterwards.
		if strings.Join(hello.SupportedProtos, ",") != "http/1.1" || len(hello.SupportedCurves) != 4 || hello.SupportedCurves[0] != tls.X25519MLKEM768 || len(hello.CipherSuites) != 17 || hello.CipherSuites[0] != tls.TLS_AES_128_GCM_SHA256 {
			t.Fatalf("unexpected CLI handshake: protocols=%v curves=%v ciphers=%v", hello.SupportedProtos, hello.SupportedCurves, hello.CipherSuites)
		}
	default:
		t.Fatalf("failed before TLS handshake: %v", err)
	}
}

func TestAuthenticatedProxyTunnel(t *testing.T) {
	t.Parallel()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "upstream.example:443" || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Errorf("CONNECT request: %s %s %v", r.Method, r.Host, r.Header)
			w.WriteHeader(http.StatusProxyAuthRequired)

			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter does not implement http.Hijacker")
			return
		}

		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()

		data := make([]byte, 4)
		if _, err := io.ReadFull(rw, data); err != nil {
			t.Error(err)
			return
		}

		_, _ = conn.Write(data)
	}))
	defer proxy.Close()

	proxyURL, _ := url.Parse(strings.Replace(proxy.URL, "://", "://user:pass@", 1))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	conn, err := dialTunnel(ctx, "tcp", "upstream.example:443", proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(time.Second))

	if _, err = conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 4)
	if _, err = io.ReadFull(conn, data); err != nil || string(data) != "ping" {
		t.Fatalf("tunnel response %q: %v", data, err)
	}
}

func TestProxyConnectCancellation(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig

	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)

		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = io.Copy(io.Discard, conn)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	proxyURL, _ := url.Parse("http://" + listener.Addr().String())

	conn, err := dialTunnel(ctx, "tcp", "upstream.example:443", proxyURL)
	if err == nil {
		_ = conn.Close()

		t.Fatal("CONNECT unexpectedly succeeded")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the proxy connection")
	}
}
