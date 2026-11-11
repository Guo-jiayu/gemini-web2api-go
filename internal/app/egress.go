package app

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type upstreamEgressError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *upstreamEgressError) Error() string { return "upstream egress blocked: " + e.Reason }

func upstreamEgressSignal(status int, location, retryAfter string) error {
	reason := ""
	if status == 429 {
		reason = "upstream_429"
	}
	if status >= 300 && status < 400 {
		u, err := url.Parse(location)
		if err == nil && (u.Hostname() == "google.com" || strings.HasSuffix(u.Hostname(), ".google.com")) && (u.Path == "/sorry" || strings.HasPrefix(u.Path, "/sorry/")) {
			reason = "google_sorry"
		}
	}
	if reason == "" {
		return nil
	}
	wait := time.Duration(0)
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds > 0 && seconds <= 86400*365 {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(retryAfter); err == nil && at.After(time.Now()) {
		wait = time.Until(at)
	}
	return &upstreamEgressError{Reason: reason, RetryAfter: wait}
}

type egressBackoff struct {
	Until     int64  `json:"blocked_until"`
	UpdatedAt int64  `json:"updated_at"`
	Failures  int    `json:"failures"`
	Reason    string `json:"reason"`
}

var egressMu sync.Mutex
var egressWriteMu sync.Mutex

// Failed DB writes still block in this process; durable state is preferred.
var egressVolatile = map[int64]egressBackoff{}

func loadEgressBackoff(id int64) (egressBackoff, error) {
	var s egressBackoff
	err := getDB().QueryRow(`SELECT blocked_until,updated_at,failures,reason FROM egress_backoff WHERE proxy_id=?`, id).Scan(&s.Until, &s.UpdatedAt, &s.Failures, &s.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	egressMu.Lock()
	local := egressVolatile[id]
	egressMu.Unlock()
	if local.Until > s.Until {
		s = local
		err = nil
	}
	return s, err
}

func egressCooldownError(id int64, s egressBackoff) *RateLimitError {
	wait := time.Until(time.Unix(s.Until, 0))
	if wait < time.Second {
		wait = time.Second
	}
	return &RateLimitError{Reason: "egress_cooldown", ProxyID: id, RetryAfter: wait}
}

func handleEgressError(id int64, err error) *RateLimitError {
	var already *RateLimitError
	if errors.As(err, &already) && strings.HasPrefix(already.Reason, "egress_") {
		return already
	}
	var signal *upstreamEgressError
	if !errors.As(err, &signal) {
		var page *appPageStatusError
		if !errors.As(err, &page) {
			return nil
		}
		location := page.Location
		if location != "" {
			location = "https://" + location
		}
		found := upstreamEgressSignal(page.Status, location, "")
		if !errors.As(found, &signal) {
			return nil
		}
		signal.RetryAfter = page.RetryAfter
	}
	if signal.Reason == "google_sorry" && rtCfg().DisableSorryCooldown && signal.RetryAfter <= 0 {
		logf("[egress] 出口 #%d 收到 google_sorry；固定冷却已关闭，直接返回上游错误", id)
		return &RateLimitError{Reason: "egress_google_sorry", ProxyID: id}
	}
	egressWriteMu.Lock()
	defer egressWriteMu.Unlock()
	old, readErr := loadEgressBackoff(id)
	if readErr != nil {
		logf("[egress] 读取出口 #%d 冷却状态失败: %v", id, readErr)
	}
	wait := time.Minute
	if signal.Reason == "google_sorry" {
		wait = time.Duration(rtCfg().ProxyCooldownMin) * time.Minute
		if wait < time.Minute {
			wait = 10 * time.Minute
		}
		if rtCfg().DisableSorryCooldown {
			wait = 0 // Only the upstream Retry-After remains mandatory.
		}
	} else {
		for i := 0; i < old.Failures && wait < 15*time.Minute; i++ {
			wait *= 2
		}
		if wait > 15*time.Minute {
			wait = 15 * time.Minute
		}
	}
	if signal.RetryAfter > wait {
		wait = signal.RetryAfter
	}
	now := time.Now()
	state := egressBackoff{Until: rotationDeadline(now, wait), UpdatedAt: now.UnixNano(), Failures: old.Failures + 1, Reason: signal.Reason}
	if old.Until > state.Until {
		state.Until = old.Until
	}
	// Publish before writing so concurrent callers cannot slip through on DB errors.
	egressMu.Lock()
	egressVolatile[id] = state
	egressMu.Unlock()
	_, writeErr := getDB().Exec(upsert("egress_backoff", []string{"proxy_id", "blocked_until", "updated_at", "failures", "reason"}, []string{"proxy_id"}), id, state.Until, state.UpdatedAt, state.Failures, state.Reason)
	if writeErr != nil {
		logf("[egress] 出口 #%d 冷却持久化失败，进程内仍暂停: %v", id, writeErr)
	}
	logf("[egress] 出口 #%d 因 %s 暂停，冷却至 %s", id, state.Reason, time.Unix(state.Until, 0).Format(time.RFC3339))
	return egressCooldownError(id, state)
}

func egressIDForURL(proxyURL string) (int64, bool) {
	if proxyURL == "" {
		return 0, true
	}
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	for _, p := range proxyCache {
		if p.URL == proxyURL {
			return p.ID, true
		}
	}
	return 0, false
}

func guardEgressURL(proxyURL string) error {
	if id, known := egressIDForURL(proxyURL); known {
		if e := currentEgressError(id); e != nil {
			return e
		}
	}
	return nil
}

func clearEgressBackoff(id int64, requestStarted time.Time) {
	egressWriteMu.Lock()
	defer egressWriteMu.Unlock()
	// An older in-flight success cannot undo a newer block from another request.
	if _, err := getDB().Exec(`DELETE FROM egress_backoff WHERE proxy_id=? AND updated_at<=?`, id, requestStarted.UnixNano()); err != nil {
		logf("[egress] 清理出口 #%d 冷却失败: %v", id, err)
		return
	}
	egressMu.Lock()
	if egressVolatile[id].UpdatedAt <= requestStarted.UnixNano() {
		delete(egressVolatile, id)
	}
	egressMu.Unlock()
}

func currentEgressError(id int64) *RateLimitError {
	s, err := loadEgressBackoff(id)
	if err == nil && s.Until > time.Now().Unix() {
		return egressCooldownError(id, s)
	}
	return nil
}

func slotLimitError(reason string, id int64) *RateLimitError {
	e := &RateLimitError{Reason: reason, ProxyID: id}
	if reason == "egress_cooldown" || reason == "egress_probe" {
		e.RetryAfter = time.Second
		candidates := []int64{id}
		if id < 0 {
			candidates = nil
			for _, p := range listProxies() {
				if p.Enabled {
					candidates = append(candidates, p.ID)
				}
			}
		}
		for _, candidate := range candidates {
			if s, err := loadEgressBackoff(candidate); err == nil && s.Until > time.Now().Unix() {
				wait := time.Until(time.Unix(s.Until, 0))
				if e.RetryAfter == time.Second || wait < e.RetryAfter {
					e.RetryAfter = wait
				}
			}
		}
	}
	return e
}

func (e *RateLimitError) setRetryAfter(w http.ResponseWriter) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
}
func (e *RateLimitError) code() string {
	if e.Reason == "egress_google_sorry" {
		return "upstream_google_sorry"
	}
	if strings.HasPrefix(e.Reason, "egress_") {
		return "egress_cooldown"
	}
	return "ip_slot_full"
}
func writeRateLimitError(w http.ResponseWriter, e *RateLimitError) {
	e.setRetryAfter(w)
	writeJSON(w, 429, map[string]interface{}{"error": map[string]string{"message": e.Error(), "type": "rate_limit_exceeded", "code": e.code()}})
}

func callErrorStatus(err error) int {
	var rate *RateLimitError
	var downgraded *ModelDowngradedError
	var prompt *PromptTooLongError
	switch {
	case errors.As(err, &rate), errors.As(err, &downgraded):
		return 429
	case errors.As(err, &prompt):
		return 400
	default:
		return 502
	}
}

func egressErrorMessage(e *RateLimitError) string {
	if e.Reason == "egress_google_sorry" {
		return fmt.Sprintf("upstream Google /sorry blocked egress #%d; local fixed cooldown is disabled", e.ProxyID)
	}
	return fmt.Sprintf("egress #%d is cooling down (%s); retry after %s", e.ProxyID, e.Reason, e.RetryAfter.Round(time.Second))
}

func callErrorDetails(err error) map[string]string {
	result := map[string]string{"message": err.Error(), "type": "upstream_error"}
	var rate *RateLimitError
	if errors.As(err, &rate) {
		result["type"] = "rate_limit_exceeded"
		result["code"] = rate.code()
		if rate.RetryAfter > 0 {
			result["retry_after"] = strconv.Itoa(int(math.Ceil(rate.RetryAfter.Seconds())))
		}
	}
	return result
}
