package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func rotationTestAccount(t *testing.T) CookieAccount {
	t.Helper()
	id, err := accountAdd("rotation-flow", "SAPISID=stable; __Secure-1PSID=stable; __Secure-1PSIDTS=old; SIDCC=old", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = getDB().Exec(`UPDATE accounts SET proxy_id=? WHERE id=?`, 998123, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = accountDelete(id)
		_, _ = getDB().Exec(`DELETE FROM account_rotations WHERE account_id=?`, id)
	})
	return *accountByID(id)
}

func TestRotateAccountSinglePostAndPersistentCooldown(t *testing.T) {
	var posts atomic.Int32
	rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		if r.Method != "POST" {
			t.Error("unexpected iframe request")
		}
		return rotationTestResponse(200, "[]", "__Secure-1PSIDTS=new", "__Secure-1PSIDCC=new"), nil
	})
	a := rotationTestAccount(t)
	if _, names, err := rotateAccount(a); err != nil || len(names) != 2 {
		t.Fatalf("names=%v err=%v", names, err)
	}
	s, err := loadRotationState(a.ID)
	if err != nil || s.LastSuccess == 0 || s.NextAttempt < s.LastSuccess+600 || s.LeaseUntil != 0 {
		t.Fatalf("missing persisted success: %+v %v", s, err)
	}
	// Use only a freshly read account and DB state: no in-memory throttle exists.
	_, _, err = rotateAccount(*accountByID(a.ID))
	var deferred *rotationDeferredError
	if !errors.As(err, &deferred) || posts.Load() != 1 {
		t.Fatalf("duplicate POST after success: posts=%d err=%v", posts.Load(), err)
	}
}

func TestRotateAccountConcurrentRequestsShareLease(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var posts atomic.Int32
	rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		entered <- struct{}{}
		<-release
		return rotationTestResponse(200, "[]", "__Secure-1PSIDTS=new"), nil
	})
	a := rotationTestAccount(t)
	done := make(chan error, 1)
	go func() { _, _, err := rotateAccount(a); done <- err }()
	<-entered
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := rotateAccount(a)
			var deferred *rotationDeferredError
			if !errors.As(err, &deferred) {
				t.Errorf("expected deferred: %v", err)
			}
		}()
	}
	wg.Wait()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatalf("sent %d POSTs", posts.Load())
	}
}

func TestRotateAccount429PersistsPartialCookieAndBackoff(t *testing.T) {
	posts := 0
	rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		posts++
		return rotationTestResponse(429, ")]}'\n[[\"identity.hfcr\",1800]]", "__Secure-1PSIDCC=partial"), nil
	})
	a := rotationTestAccount(t)
	wait, names, err := rotateAccount(a)
	var limited *rotationHTTPError
	if !errors.As(err, &limited) || wait != 30*time.Minute || len(names) != 1 {
		t.Fatalf("wait=%s names=%v err=%v", wait, names, err)
	}
	if cookieValue(accountByID(a.ID).Cookie, "__Secure-1PSIDCC") != "partial" {
		t.Fatal("partial cookie not persisted")
	}
	s, _ := loadRotationState(a.ID)
	if s.LastSuccess != 0 || s.Failures != 1 || s.LastError == "" || s.NextAttempt < s.LastAttempt+1800 {
		t.Fatalf("bad rate-limit state: %+v", s)
	}
	_, _, err = rotateAccount(a)
	var deferred *rotationDeferredError
	if !errors.As(err, &deferred) || posts != 1 {
		t.Fatal("retry during server cooldown")
	}
}

func TestRotationLeaseCanRecoverAfterCrash(t *testing.T) {
	a := rotationTestAccount(t)
	_, token, err := claimRotation(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := claimRotation(a.ID); err == nil {
		t.Fatal("duplicate live lease")
	}
	_, _ = getDB().Exec(`UPDATE account_rotations SET lease_until=? WHERE account_id=?`, time.Now().Add(-time.Second).Unix(), a.ID)
	s, newToken, err := claimRotation(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishRotation(a.ID, token, s, time.Minute, nil); err == nil {
		t.Fatal("stale owner could release new lease")
	}
	if err := finishRotation(a.ID, newToken, s, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRotationFailureBackoff(t *testing.T) {
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		if got := rotationFailureWait(rotationState{Failures: i}, errors.New("network failure")); got != want {
			t.Fatalf("failures=%d got=%s want=%s", i, got, want)
		}
	}
}

func TestRotationDeadlineDoesNotShortenRetryAfter(t *testing.T) {
	for _, nano := range []int64{0, 1, 123456789, 999999999} {
		now := time.Unix(1800000000, nano)
		deadline := time.Unix(rotationDeadline(now, 10*time.Minute), 0)
		if deadline.Before(now.Add(10*time.Minute)) || deadline.Sub(now.Add(10*time.Minute)) >= time.Second {
			t.Fatalf("deadline rounds incorrectly for nanoseconds=%d", nano)
		}
	}
}

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
