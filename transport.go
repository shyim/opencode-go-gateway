package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
)

// OpenCode's released CLI uses Bun's BoringSSL HTTP/1.1 handshake. The upstream
// treats Go's default TLS handshake differently even with identical HTTP headers.
// Keep the profile explicit: browser profiles do not match the CLI either.
func cliHello() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030,
			0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x009c,
			0x009d, 0x002f, 0x0035,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501,
				0x0806, 0x0601, 0x0807, 0x0603, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

type cliTransport struct {
	https, plain *http.Transport
}

func newCLITransport() *cliTransport {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		defaultTransport = &http.Transport{}
	}

	plain := defaultTransport.Clone()
	secure := plain.Clone()
	// CONNECT must happen before our TLS handshake, including when a proxy is set.
	secure.Proxy = nil
	secure.ForceAttemptHTTP2 = false
	secure.DialTLSContext = dialCLITLS

	return &cliTransport{https: secure, plain: plain}
}

func (t *cliTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == schemeHTTPS {
		return t.https.RoundTrip(r)
	}

	return t.plain.RoundTrip(r)
}

func (t *cliTransport) CloseIdleConnections() {
	t.https.CloseIdleConnections()
	t.plain.CloseIdleConnections()
}

func dialCLITLS(ctx context.Context, network, address string) (net.Conn, error) {
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: schemeHTTPS, Host: address}})
	if err != nil {
		return nil, err
	}

	conn, err := dialTunnel(ctx, network, address, proxyURL)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	client := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err = client.ApplyPreset(cliHello()); err == nil {
		handshake, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		err = client.HandshakeContext(handshake)
	}

	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	return client, nil
}

func proxyAddress(proxyURL *url.URL) (string, error) {
	if proxyURL.Scheme != "http" && proxyURL.Scheme != schemeHTTPS {
		return "", fmt.Errorf("unsupported HTTPS proxy scheme %q", proxyURL.Scheme)
	}

	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if proxyURL.Scheme == schemeHTTPS {
			port = "443"
		}
	}

	return net.JoinHostPort(proxyURL.Hostname(), port), nil
}

func proxyConnectRequest(address string, proxyURL *url.URL) *http.Request {
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address},
		Host:   address,
		Header: make(http.Header),
	}

	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credential := proxyURL.User.Username() + ":" + password
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credential)))
	}

	return req
}

func dialTunnel(ctx context.Context, network, address string, proxyURL *url.URL) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if proxyURL == nil {
		return dialer.DialContext(ctx, network, address)
	}

	targetAddress, err := proxyAddress(proxyURL)
	if err != nil {
		return nil, err
	}

	conn, err := dialer.DialContext(ctx, network, targetAddress)
	if err != nil {
		return nil, err
	}
	// Bound proxy TLS and CONNECT, and interrupt both promptly on cancellation.
	rawConn := conn

	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()

	deadline := time.Now().Add(30 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}

	_ = conn.SetDeadline(deadline)

	fail := func(err error) (net.Conn, error) { _ = conn.Close(); return nil, err }

	if proxyURL.Scheme == schemeHTTPS {
		secured := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if err := secured.HandshakeContext(ctx); err != nil {
			return fail(err)
		}

		conn = secured
	}

	req := proxyConnectRequest(address, proxyURL)
	if err = req.Write(conn); err != nil {
		return fail(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("HTTPS proxy CONNECT: HTTP %d", resp.StatusCode))
	}

	_ = conn.SetDeadline(time.Time{})

	return conn, nil
}
