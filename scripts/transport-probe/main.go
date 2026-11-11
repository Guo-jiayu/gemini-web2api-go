// transport-probe compares a bounded set of /app requests through the configured
// proxy. It never prints cookies, response bodies, or redirect query strings.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "file:data/gemini.db?mode=ro")
	if err != nil {
		panic("open database failed")
	}
	defer db.Close()
	var proxy, cookie string
	var account, authuser int
	if db.QueryRow("SELECT url FROM proxies WHERE enabled=1 ORDER BY id LIMIT 1").Scan(&proxy) != nil {
		panic("missing enabled proxy")
	}
	if db.QueryRow("SELECT id,cookie,authuser FROM accounts WHERE status='enabled' ORDER BY id LIMIT 1").Scan(&account, &cookie, &authuser) != nil {
		panic("missing enabled account")
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		panic("invalid proxy URL")
	}
	target := "https://gemini.google.com/app"
	if authuser > 0 {
		target = fmt.Sprintf("https://gemini.google.com/u/%d/app", authuser)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, signed := range []bool{false, true} {
		for _, navigation := range []bool{false, true} {
			for _, browserTLS := range []bool{false, true} {
				headers := map[string]string{
					"User-Agent":       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
					"Accept":           "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
					"Accept-Language":  "en-US,en;q=0.9",
					"Sec-CH-UA":        `"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="24"`,
					"Sec-CH-UA-Mobile": "?0", "Sec-CH-UA-Platform": `"Windows"`,
					"Sec-Fetch-Dest": "empty", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin",
				}
				if signed {
					headers["Cookie"] = cookie
				}
				if navigation {
					headers["Sec-Fetch-Dest"] = "document"
					headers["Sec-Fetch-Mode"] = "navigate"
					headers["Sec-Fetch-Site"] = "none"
					headers["Sec-Fetch-User"] = "?1"
					headers["Upgrade-Insecure-Requests"] = "1"
				}
				row := map[string]any{"at": time.Now().Format(time.RFC3339), "signed_in_cookie": signed, "navigation_headers": navigation, "browser_tls_client": browserTLS}
				if signed {
					row["account_id"] = account
				}
				start := time.Now()
				var status int
				var location, protocol, retryAfter string
				var body io.ReadCloser
				var requestErr error
				if browserTLS {
					c, e := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), tlsclient.WithTimeoutSeconds(15), tlsclient.WithClientProfile(profiles.Chrome_146), tlsclient.WithProxyUrl(proxy), tlsclient.WithNotFollowRedirects())
					if e != nil {
						panic("TLS client setup failed")
					}
					r, _ := fhttp.NewRequest("GET", target, nil)
					for k, v := range headers {
						r.Header.Set(k, v)
					}
					resp, e := c.Do(r)
					requestErr = e
					if e == nil {
						status, location, protocol, retryAfter, body = resp.StatusCode, resp.Header.Get("Location"), resp.Proto, resp.Header.Get("Retry-After"), resp.Body
					}
					defer c.CloseIdleConnections()
				} else {
					t := &http.Transport{Proxy: http.ProxyURL(proxyURL), ForceAttemptHTTP2: true}
					c := &http.Client{Transport: t, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
					r, _ := http.NewRequest("GET", target, nil)
					for k, v := range headers {
						r.Header.Set(k, v)
					}
					resp, e := c.Do(r)
					requestErr = e
					if e == nil {
						status, location, protocol, retryAfter, body = resp.StatusCode, resp.Header.Get("Location"), resp.Proto, resp.Header.Get("Retry-After"), resp.Body
					}
					defer t.CloseIdleConnections()
				}
				if requestErr != nil {
					row["error_type"] = fmt.Sprintf("%T", requestErr)
				} else {
					data, e := io.ReadAll(io.LimitReader(body, 8<<20))
					body.Close()
					loc, _ := url.Parse(location)
					row["status"], row["protocol"], row["body_bytes"] = status, protocol, len(data)
					row["has_session_token"] = strings.Contains(string(data), "SNlM0e")
					row["body_read_ok"] = e == nil
					if loc != nil {
						row["location_host"], row["location_path"] = loc.Hostname(), loc.Path
					}
				}
				row["seconds"] = time.Since(start).Seconds()
				enc.Encode(row)
				if status == 429 || retryAfter != "" {
					return
				}
				time.Sleep(2 * time.Second)
			}
		}
	}
}
