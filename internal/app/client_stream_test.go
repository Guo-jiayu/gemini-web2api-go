package app

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestConcurrentRequestsShareOneClientPerEgress(t *testing.T) {
	const proxy = "http://pool-cache-test.invalid:1234"
	key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
	stdlibClientCache.Delete(key)
	defer stdlibClientCache.Delete(key)
	start := make(chan struct{})
	clients := make(chan *http.Client, 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; clients <- getStdlibClient(proxy) }()
	}
	close(start)
	wg.Wait()
	close(clients)
	var first *http.Client
	for client := range clients {
		if first == nil {
			first = client
		}
		if client != first {
			t.Fatal("concurrent requests created separate connection pools for the same egress")
		}
	}
	other := getStdlibClient(proxy + "0")
	defer stdlibClientCache.Delete(fmt.Sprintf("%s0|%d", proxy, rtCfg().RequestTimeout))
	if other == first {
		t.Fatal("different egresses shared a client")
	}
}

// Verify both ALPN multiplexing through CONNECT and HTTP/1.1 fallback, with
// a server that cannot finish until the client has received the first line.
func TestProxyNegotiatesProtocolAndStreamsBeforeEOF(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", h2), func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Test-Protocol", r.Proto)
				fmt.Fprint(w, "first\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			origin.EnableHTTP2 = h2
			origin.StartTLS()
			defer origin.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != origin.Listener.Addr().String() {
					http.Error(w, "unexpected destination", http.StatusBadRequest)
					return
				}
				upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
				if err != nil {
					http.Error(w, "dial failed", 502)
					return
				}
				defer upstream.Close()
				client, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer client.Close()
				fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() { _, _ = io.Copy(upstream, client) }()
				_, _ = io.Copy(client, upstream)
			}))
			defer proxy.Close()
			client := getStdlibClient(proxy.URL)
			client.Timeout = 5 * time.Second
			transport := client.Transport.(*http.Transport)
			defer transport.CloseIdleConnections()
			defer stdlibClientCache.Delete(fmt.Sprintf("%s|%d", proxy.URL, rtCfg().RequestTimeout))
			roots := x509.NewCertPool()
			roots.AddCert(origin.Certificate())
			transport.TLSClientConfig = &tls.Config{RootCAs: roots}
			resp, err := client.Get(origin.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			want := 1
			if h2 {
				want = 2
			}
			if resp.ProtoMajor != want {
				t.Fatalf("protocol = %s, want HTTP/%d", resp.Proto, want)
			}
			line, err := bufio.NewReader(resp.Body).ReadString('\n')
			if err != nil || line != "first\n" {
				t.Fatalf("first line before EOF: %q, %v", line, err)
			}
		})
	}
}
