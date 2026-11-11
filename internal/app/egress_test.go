package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func clearTestEgress(t *testing.T, id int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = getDB().Exec(`DELETE FROM egress_backoff WHERE proxy_id=?`, id)
		egressMu.Lock()
		delete(egressVolatile, id)
		egressMu.Unlock()
		slotsMu.Lock()
		delete(slots, id)
		slotsMu.Unlock()
	})
}

func TestEgressSignalClassification(t *testing.T) {
	for _, tc := range []struct {
		status   int
		location string
		want     bool
	}{
		{302, "https://www.google.com/sorry/index?secret=hidden", true},
		{302, "https://accounts.google.com/ServiceLogin", false},
		{302, "https://google.com.attacker.invalid/sorry/index", false},
		{403, "", false}, {429, "", true}, {200, "", false},
	} {
		signal := upstreamEgressSignal(tc.status, tc.location, "120")
		if (signal != nil) != tc.want {
			t.Fatalf("status=%d location=%s signal=%v", tc.status, tc.location, signal)
		}
		if signal != nil && strings.Contains(signal.Error(), "secret") {
			t.Fatal("query leaked")
		}
	}
}

func TestSorryCooldownDisabled(t *testing.T) {
	const id int64 = 990091
	withTestProxySlot(t, Proxy{ID: id, URL: "http://disabled-sorry.invalid", Enabled: true}, func(c *RuntimeConfig) {
		c.DisableSorryCooldown = true
		c.ProxyCooldownMin = 120
	})
	clearTestEgress(t, id)
	signal := upstreamEgressSignal(302, "https://www.google.com/sorry/index", "")
	err := handleEgressError(id, signal)
	if err == nil || err.code() != "upstream_google_sorry" || err.RetryAfter != 0 {
		t.Fatalf("expected upstream rejection without local pause: %v", err)
	}
	state, loadErr := loadEgressBackoff(id)
	if loadErr != nil || state.Until != 0 || currentEgressError(id) != nil {
		t.Fatalf("disabled pause persisted: %+v, %v", state, loadErr)
	}
	if handleEgressError(id, err) != err {
		t.Fatal("reclassified upstream rejection")
	}
	w := httptest.NewRecorder()
	writeRateLimitError(w, err)
	if w.Code != 429 || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "upstream_google_sorry") {
		t.Fatalf("misleading cooldown response: %d %s", w.Code, w.Body.String())
	}
}

func TestDisabledSorryCooldownHonorsRetryAfterAndHTTP429(t *testing.T) {
	const id int64 = 990092
	withTestProxySlot(t, Proxy{ID: id, URL: "http://sorry-retry.invalid", Enabled: true}, func(c *RuntimeConfig) {
		c.DisableSorryCooldown = true
		c.ProxyCooldownMin = 120
	})
	clearTestEgress(t, id)
	err := handleEgressError(id, upstreamEgressSignal(302, "https://www.google.com/sorry/index", "90"))
	if err == nil || err.RetryAfter < 90*time.Second || err.RetryAfter > 92*time.Second {
		t.Fatalf("must honor upstream wait without applying 2 hours: %v", err)
	}
	clearEgressBackoff(id, time.Now())
	err = handleEgressError(id, upstreamEgressSignal(429, "", "180"))
	if err == nil || err.RetryAfter < 180*time.Second || currentEgressError(id) == nil {
		t.Fatal("ordinary upstream 429 was bypassed")
	}
}

func TestEgressSorryStopsAccountRetriesAndLogs429(t *testing.T) {
	var calls atomic.Int32
	rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		resp := rotationTestResponse(302, "")
		resp.Header.Set("Location", "https://www.google.com/sorry/index?secret=hidden")
		return resp, nil
	})
	clearTestEgress(t, 998123)
	ids := fairnessAccounts(t, 3)
	for _, id := range ids {
		_, _ = getDB().Exec(`UPDATE accounts SET proxy_id=? WHERE id=?`, 998123, id)
		invalidateXSRF(accountByID(id).Cookie)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"egress-test"}]}`))
		w := httptest.NewRecorder()
		handleChatCompletions(w, req)
		wait, _ := strconv.Atoi(w.Header().Get("Retry-After"))
		if w.Code != 429 || wait < 7200 || !strings.Contains(w.Body.String(), "egress_cooldown") {
			t.Fatalf("HTTP %d retry=%d body=%s", w.Code, wait, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("continued upstream/account retries after sorry: %d", calls.Load())
	}
	for _, id := range ids {
		if accountByID(id).FailCount != 0 {
			t.Fatal("egress block damaged account health")
		}
	}
	var logged int
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		_ = getDB().QueryRow(`SELECT status FROM requests WHERE error LIKE '%egress_cooldown%' ORDER BY id DESC LIMIT 1`).Scan(&logged)
		if logged == 429 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	if logged != 429 {
		t.Fatalf("recorded status=%d, expected429", logged)
	}
}

func TestEgress429PersistenceAndNoDirectFallback(t *testing.T) {
	const id int64 = 990083
	withTestProxySlot(t, Proxy{ID: id, URL: "http://blocked.invalid", Enabled: true}, func(c *RuntimeConfig) { c.FallbackDirect = true })
	clearTestEgress(t, id)
	err := handleEgressError(id, upstreamEgressSignal(429, "", "180"))
	if err == nil || err.RetryAfter < 180*time.Second {
		t.Fatal("ignored Retry-After")
	}
	// Drop volatile state to simulate restart: the database must still block.
	egressMu.Lock()
	delete(egressVolatile, id)
	egressMu.Unlock()
	_, ok, err2 := acquireSlot(id)
	var limited *RateLimitError
	if ok || !errors.As(err2, &limited) || limited.code() != "egress_cooldown" {
		t.Fatalf("bypassed persisted cooldown: %v", err2)
	}
	if slotUsage(0).Inflight != 0 {
		t.Fatal("silently used direct route")
	}
}

func TestEgressHalfOpenAllowsOnlyOneProbe(t *testing.T) {
	const id int64 = 990084
	withTestProxySlot(t, Proxy{ID: id, URL: "http://probe.invalid", Enabled: true}, nil)
	clearTestEgress(t, id)
	_, err := getDB().Exec(upsert("egress_backoff", []string{"proxy_id", "blocked_until", "updated_at", "failures", "reason"}, []string{"proxy_id"}), id, time.Now().Add(-time.Second).Unix(), time.Now().Add(-time.Minute).UnixNano(), 1, "upstream_429")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := trySlotAcquire(id); ok {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("half-open admitted %d probes", accepted.Load())
	}
	clearEgressBackoff(id, time.Now())
	releaseSlot(id)
	if ok, reason := trySlotAcquire(id); !ok {
		t.Fatalf("successful recovery didn't reopen: %s", reason)
	}
	releaseSlot(id)
}

func TestEgressOldSuccessCannotClearNewBlock(t *testing.T) {
	const id int64 = 990085
	clearTestEgress(t, id)
	started := time.Now().Add(-time.Minute)
	handleEgressError(id, upstreamEgressSignal(429, "", "120"))
	clearEgressBackoff(id, started)
	if currentEgressError(id) == nil {
		t.Fatal("old success cleared newer risk block")
	}
}

func TestEgressTransportPreservesRetryAfter(t *testing.T) {
	p := rotationTestClient(t, func(r *http.Request) (*http.Response, error) {
		resp := rotationTestResponse(429, "busy")
		resp.Header.Set("Retry-After", "360")
		return resp, nil
	})
	clearTestEgress(t, 998123)
	_, _, _, _, err := doGeminiRequest("https://gemini.google.com/generate", "", nil, p, nil)
	var signal *upstreamEgressError
	if !errors.As(err, &signal) || signal.RetryAfter != 6*time.Minute {
		t.Fatalf("legacy transport lost risk headers: %v", err)
	}
	_, err = wcSend(context.Background(), "POST", "https://gemini.google.com/wc", "", "", p)
	if !errors.As(err, &signal) || signal.RetryAfter != 6*time.Minute {
		t.Fatalf("WebChannel lost risk headers: %v", err)
	}
}

func TestEgressErrorResponses(t *testing.T) {
	err := &RateLimitError{Reason: "egress_cooldown", ProxyID: 1, RetryAfter: 90 * time.Second}
	for _, kind := range []string{"openai", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			w := httptest.NewRecorder()
			if kind == "openai" {
				writeRateLimitError(w, err)
			} else {
				writeGeminiCallError(w, err)
			}
			if w.Code != 429 || w.Header().Get("Retry-After") != "90" {
				t.Fatalf("bad downstream response: %d %v", w.Code, w.Header())
			}
		})
	}
	if callErrorStatus(fmt.Errorf("wrapped: %w", err)) != 429 {
		t.Fatal("wrapped rate limit logged incorrectly")
	}
	details := callErrorDetails(err)
	if details["code"] != "egress_cooldown" || details["retry_after"] != "90" {
		t.Fatal("streaming failure lacks retry metadata")
	}
}
