package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rotationTestTransport func(*http.Request) (*http.Response, error)

func (f rotationTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func rotationTestResponse(status int, body string, cookies ...string) *http.Response {
	h := http.Header{}
	for _, c := range cookies {
		h.Add("Set-Cookie", c)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}
func rotationTestClient(t *testing.T, f rotationTestTransport) string {
	t.Helper()
	p := "http://rotation-test.invalid:12345"
	withTestProxySlot(t, Proxy{ID: 998123, URL: p, Enabled: true}, func(c *RuntimeConfig) { c.RequestTimeout = 2 })
	key := fmt.Sprintf("%s|%d", p, rtCfg().RequestTimeout)
	stdlibClientCache.Store(key, &http.Client{Transport: f})
	t.Cleanup(func() { stdlibClientCache.Delete(key) })
	return p
}

func TestRotateSIDCCPreservesPageCookiesOn429(t *testing.T) {
	p := rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return rotationTestResponse(200, "init('12345678',658.0,0.0,0.0,1800.0)", "SIDCC=page-new; Secure"), nil
		}
		result := rotationTestResponse(429, ")]}'\n[[\"identity.hfcr\",2400]]", "__Secure-1PSIDCC=post-new; Secure")
		result.Header.Set("Retry-After", "3600")
		return result, nil
	})
	cookie, iv, names, err := rotateSIDCC("SIDCC=old", p)
	var limited *rotationHTTPError
	if !errors.As(err, &limited) || iv != time.Hour || len(names) != 2 || cookieValue(cookie, "SIDCC") != "page-new" || cookieValue(cookie, "__Secure-1PSIDCC") != "post-new" {
		t.Fatalf("lost partial response or backoff: iv=%s names=%v err=%v", iv, names, err)
	}
}
func TestRotateSIDCCPreservesPageCookiesOnTransportFailure(t *testing.T) {
	p := rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return rotationTestResponse(200, "init('12345678',658.0,0.0,0.0,1800.0)", "SIDCC=page-new"), nil
		}
		return nil, errors.New("connection reset")
	})
	cookie, iv, names, err := rotateSIDCC("SIDCC=old", p)
	if err == nil || iv != 30*time.Minute || len(names) != 1 || cookieValue(cookie, "SIDCC") != "page-new" {
		t.Fatal("page result lost after POST failure")
	}
}
func TestRotateSIDCCPreservesPageCookiesWithoutInit(t *testing.T) {
	p := rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		return rotationTestResponse(200, "no init", "SIDCC=page-new"), nil
	})
	cookie, _, names, err := rotateSIDCC("SIDCC=old", p)
	if err == nil || len(names) != 1 || cookieValue(cookie, "SIDCC") != "page-new" {
		t.Fatal("page result lost on parse failure")
	}
}
func TestRotateRejectsEmptyRenewal(t *testing.T) {
	p := rotationTestClient(t, func(r *http.Request) (*http.Response, error) { return rotationTestResponse(200, "[]"), nil })
	if _, _, err := rotate1PSIDTS("__Secure-1PSID=id; __Secure-1PSIDTS=old", p); err == nil {
		t.Fatal("empty renewal reported success")
	}
}
func TestRotationRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		header, body string
		want         time.Duration
	}{
		{"", ")]}'\n[[\"identity.hfcr\",1800],[\"di\",11]]", 30 * time.Minute},
		{"1200", ")]}'\n[[\"identity.hfcr\",600]]", 20 * time.Minute},
		{now.Add(time.Hour).Format(http.TimeFormat), "", time.Hour},
		{"garbage", "not json", 10 * time.Minute},
		{"-1", "[]", 10 * time.Minute},
	} {
		if got := rotationRetryAfter(tc.header, []byte(tc.body), now); got != tc.want {
			t.Errorf("got %s want %s", got, tc.want)
		}
	}
}
