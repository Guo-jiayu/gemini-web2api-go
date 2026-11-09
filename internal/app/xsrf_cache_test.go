package app

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestXSRFStableSessionKey(t *testing.T) {
	a := "SID=s; SAPISID=a; __Secure-1PSID=p; SIDCC=old; __Secure-1PSIDTS=old"
	b := "__Secure-1PSIDTS=new; __Secure-1PSID=p; SAPISID=a; SIDCC=new; SID=s"
	if xsrfKey(a, 0) != xsrfKey(b, 0) {
		t.Fatal("rotating cookies or their order invalidated the session key")
	}
	for _, other := range []string{"SID=s; SAPISID=b; __Secure-1PSID=p", "SID=s; SAPISID=a; __Secure-1PSID=q"} {
		if xsrfKey(a, 0) == xsrfKey(other, 0) {
			t.Fatal("changed login credentials shared cached tokens")
		}
	}
}

func TestAppTokensCoalesceAndReuseAfterRotation(t *testing.T) {
	cookie := "SAPISID=coalesce; __Secure-1PSID=session"
	invalidateXSRF(cookie)
	t.Cleanup(func() { invalidateXSRF(cookie) })
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	fetch := func(string, int, string) (xsrfEntry, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return xsrfEntry{token: "token", pushID: "push", pctx: "context", serverToken: "browser-session", fetched: time.Now()}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := cachedAppTokens(cookie, 0, "proxy-a", fetch)
			if err != nil || e.token != "token" || e.pushID != "push" || e.serverToken != "browser-session" {
				t.Errorf("entry=%+v err=%v", e, err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d page fetches for the same session", calls.Load())
	}
	_, _ = cachedAppTokens(cookie+"; SIDCC=rotated", 0, "proxy-a", fetch)
	if calls.Load() != 1 {
		t.Fatal("rotation caused another fetch")
	}
	_, _ = cachedAppTokens(cookie, 0, "proxy-b", fetch)
	if calls.Load() != 2 {
		t.Fatal("different egress reused the same cache entry")
	}
	invalidateXSRF(cookie)
	_, _ = cachedAppTokens(cookie, 0, "proxy-a", fetch)
	if calls.Load() != 3 {
		t.Fatal("explicit invalidation did not refetch")
	}
}

func TestAppTokensInvalidationDuringFetch(t *testing.T) {
	cookie := "SAPISID=inflight; __Secure-1PSID=session"
	invalidateXSRF(cookie)
	t.Cleanup(func() { invalidateXSRF(cookie) })
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(string, int, string) (xsrfEntry, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return xsrfEntry{token: "stale", fetched: time.Now()}, nil
		}
		return xsrfEntry{token: "fresh", fetched: time.Now()}, nil
	}
	done := make(chan xsrfEntry, 1)
	go func() { e, _ := cachedAppTokens(cookie, 1, "proxy", fetch); done <- e }()
	<-started
	invalidateXSRF(cookie)
	close(release)
	if e := <-done; e.token != "fresh" {
		t.Fatalf("returned invalidated token: %q", e.token)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected refetch, got %d", calls.Load())
	}
}

func TestAppTokensDoesNotCacheFailures(t *testing.T) {
	cookie := "SAPISID=failed; __Secure-1PSID=session"
	invalidateXSRF(cookie)
	t.Cleanup(func() { invalidateXSRF(cookie) })
	want := errors.New("unavailable")
	_, err := cachedAppTokens(cookie, 0, "", func(string, int, string) (xsrfEntry, error) { return xsrfEntry{}, want })
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
	e, err := cachedAppTokens(cookie, 0, "", func(string, int, string) (xsrfEntry, error) {
		return xsrfEntry{token: "recovered", fetched: time.Now()}, nil
	})
	if err != nil || e.token != "recovered" {
		t.Fatalf("recovery failed: %+v %v", e, err)
	}
}

// A slow account must not hold a process-wide lock across the network call.
func TestAppTokensDifferentAccountsDoNotBlockEachOther(t *testing.T) {
	a, b := "SAPISID=slow; __Secure-1PSID=account-a", "SAPISID=fast; __Secure-1PSID=account-b"
	defer invalidateXSRF(a)
	defer invalidateXSRF(b)
	started, release := make(chan struct{}), make(chan struct{})
	doneA, doneB := make(chan xsrfEntry, 1), make(chan xsrfEntry, 1)
	defer func() {
		close(release)
		select {
		case e := <-doneA:
			if e.token != "account-a" {
				t.Error("account A received another account's token")
			}
		case <-time.After(2 * time.Second):
			t.Error("account A did not finish after release")
		}
	}()
	fetch := func(cookie string, authuser int, proxy string) (xsrfEntry, error) {
		if cookie == a {
			close(started)
			<-release
		}
		return xsrfEntry{token: cookieValue(cookie, "__Secure-1PSID"), serverToken: "server-" + cookieValue(cookie, "__Secure-1PSID"), fetched: time.Now()}, nil
	}
	go func() { e, _ := cachedAppTokens(a, 0, "same-proxy", fetch); doneA <- e }()
	<-started
	go func() { e, _ := cachedAppTokens(b, 0, "same-proxy", fetch); doneB <- e }()
	select {
	case e := <-doneB:
		if e.token != "account-b" || e.serverToken != "server-account-b" {
			t.Fatal("cross-account token reuse")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("account B was blocked by account A's page fetch")
	}
}

func TestWarmupPreservesAccountEgress(t *testing.T) {
	proxies := []Proxy{{ID: 1, URL: "http://one", Enabled: true}, {ID: 2, URL: "http://two", Enabled: true}, {ID: 3, URL: "http://disabled", Enabled: false}}
	if p, ok := warmupRoute(2, proxies); !ok || p.ID != 2 {
		t.Fatal("lost bound egress")
	}
	for _, id := range []int64{0, 3, 99} {
		if _, ok := warmupRoute(id, proxies); ok {
			t.Fatalf("warmup rerouted unbound/disabled/missing egress %d", id)
		}
	}
	if p, ok := warmupRoute(0, nil); !ok || p.ID != 0 {
		t.Fatal("direct account should warm directly without a proxy pool")
	}
	if _, ok := warmupRoute(2, nil); ok {
		t.Fatal("removed bound proxy must not silently become direct in warmup")
	}
}

func TestWarmupBoundedForLargeAccountPools(t *testing.T) {
	var accounts []CookieAccount
	var proxies []Proxy
	for id := int64(1); id <= 20; id++ {
		proxies = append(proxies, Proxy{ID: id, Enabled: true})
		for n := int64(0); n < 100; n++ {
			accounts = append(accounts, CookieAccount{ID: id*1000 + n, ProxyID: id, Status: "enabled"})
		}
	}
	chosen := warmupCandidates(accounts, proxies, 4)
	if len(chosen) != 4 {
		t.Fatalf("warmed %d accounts, want bounded startup work", len(chosen))
	}
	seen := map[int64]bool{}
	for _, a := range chosen {
		if seen[a.ProxyID] {
			t.Fatal("startup consumed multiple slots for the same egress")
		}
		seen[a.ProxyID] = true
	}
}
