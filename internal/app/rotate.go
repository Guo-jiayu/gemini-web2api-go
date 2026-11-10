package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 会话保活优先使用哨兵刷新 TS（响应也可能包含 CC），不再紧接着发送
// 第二条 RotateCookies POST。仅缺少 1PSID 的旧凭证使用 iframe 路径。
// 账号级租约、下次尝试时间和结果持久化在 account_rotations 中。

const (
	rotatePageURL = "https://accounts.google.com/RotateCookiesPage" +
		"?og_pid=658&rot=3&origin=https%3A%2F%2Fgemini.google.com&exp_id=0"
	rotatePostURL = "https://accounts.google.com/RotateCookies"
	// og_pid 是产品标识，Gemini 固定 658；它既作为上面页面的 query，也回显在页面里。
	rotateProductID = 658
	// 服务端没给出间隔时的兜底值。
	defaultRotateInterval = 10 * time.Minute
	// 启动后尽快刷一次：导入时 cookie 可能已经快到期，干等 10 分钟会直接过期。
	firstRotateDelay = 15 * time.Second
	// 哨兵 payload。前导零是故意的，见文件头。
	rotate1PSIDTSBody = `[000,"-0000000000000000000"]`
	// 打太勤会 429。Gemini-API / notebooklm-py 都用 60 秒地板。
	min1PSIDTSInterval   = 60 * time.Second
	rotateRequestTimeout = 30 * time.Second
)

type rotationHTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *rotationHTTPError) Error() string {
	return fmt.Sprintf("RotateCookies 返回 HTTP %d（下次尝试不早于 %s）", e.Status, e.RetryAfter)
}

func rotationRetryAfter(header string, body []byte, now time.Time) time.Duration {
	wait := defaultRotateInterval
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil && seconds > 0 && seconds <= int64((365*24*time.Hour)/time.Second) {
		if d := time.Duration(seconds) * time.Second; d > wait {
			wait = d
		}
	} else if at, err := http.ParseTime(header); err == nil && at.Sub(now) > wait {
		wait = at.Sub(now)
	}
	payload := strings.TrimSpace(string(body))
	if strings.HasPrefix(payload, ")]}'") {
		payload = strings.TrimSpace(payload[4:])
	}
	var rows [][]json.RawMessage
	if json.Unmarshal([]byte(payload), &rows) == nil {
		for _, row := range rows {
			if len(row) < 2 {
				continue
			}
			var tag string
			var seconds float64
			if json.Unmarshal(row[0], &tag) == nil && tag == "identity.hfcr" && json.Unmarshal(row[1], &seconds) == nil && seconds >= 60 && seconds <= 86400 {
				if d := time.Duration(seconds * float64(time.Second)); d > wait {
					wait = d
				}
			}
		}
	}
	return wait
}

// 刷新 1PSIDTS 只带这一对。多带实测会 401。
var rotate1PSIDTSCookies = []string{"__Secure-1PSID", "__Secure-1PSIDTS"}

// 页面里形如：init('4162200486104360679', 658.0, 0.0, 0.0, 600.0)
// 第一个参数是这个会话的标识，最后一个是下次轮转的间隔秒数。
var rotateInitRe = regexp.MustCompile(`init\('([^']{4,64})'\s*,\s*([0-9.]+)\s*,[^)]*?([0-9.]+)\s*\)`)

// rotateAccount is the shared entry point for scheduled, admin and recovery
// refreshes. Claim before any network operation and re-read the latest cookie.
func rotateAccount(a CookieAccount) (time.Duration, []string, error) {
	state, token, err := claimRotation(a.ID)
	if err != nil {
		var deferred *rotationDeferredError
		if errors.As(err, &deferred) {
			return deferred.Wait, nil, err
		}
		return min1PSIDTSInterval, nil, err
	}
	interval := defaultRotateInterval
	var names []string
	fresh := accountByID(a.ID)
	if fresh == nil {
		err = fmt.Errorf("account #%d no longer exists", a.ID)
	} else {
		a = *fresh
		proxyURL := ""
		if a.ProxyID > 0 {
			proxyURL = proxyURLByID(a.ProxyID)
		}
		cookie := a.Cookie
		if a.ProxyID > 0 && proxyURL == "" {
			err = fmt.Errorf("account #%d bound proxy is unavailable", a.ID)
		} else if cookieValue(cookie, "__Secure-1PSID") != "" {
			cookie, names, err = rotate1PSIDTS(cookie, proxyURL)
		} else {
			cookie, interval, names, err = rotateSIDCC(cookie, proxyURL)
		}
		if cookie != a.Cookie {
			stored, saveErr := updateAccountCookie(a.ID, a.Cookie, cookie)
			if saveErr != nil {
				err = errors.Join(err, fmt.Errorf("保活 Cookie 写回失败: %w", saveErr))
				names = nil
			} else {
				actual := []string{}
				for _, name := range uniqueKeepOrder(names) {
					if cookieValue(stored, name) != cookieValue(a.Cookie, name) {
						actual = append(actual, name)
					}
				}
				names = actual
				if xsrfKey(a.Cookie, a.AuthUser) != xsrfKey(stored, a.AuthUser) {
					invalidateXSRF(a.Cookie)
				}
			}
		} else {
			names = nil
		}
		if err == nil && len(names) == 0 {
			err = fmt.Errorf("保活未取得有效的新 Cookie")
		}
	}
	if err != nil {
		backoff := rotationFailureWait(state, err)
		if interval <= defaultRotateInterval || backoff > interval {
			interval = backoff
		}
	}
	if interval < min1PSIDTSInterval {
		interval = min1PSIDTSInterval
	}
	if saveErr := finishRotation(a.ID, token, state, interval, err); saveErr != nil {
		return interval, names, errors.Join(err, saveErr)
	}
	if len(names) > 0 {
		logf("[rotate] 账号 #%d 已保存 %s；下次检查 %s 后", a.ID, strings.Join(names, ", "), interval.Round(time.Second))
	}
	return interval, names, err
}

// rotate1PSIDTS POST 哨兵 payload，把响应里的 Set-Cookie 合并回完整 cookie 串。
func rotate1PSIDTS(cookie, proxyURL string) (string, []string, error) {
	subset := cookieSubset(cookie, rotate1PSIDTSCookies)
	headers := map[string]string{
		"Accept":         "*/*",
		"Content-Type":   "application/json",
		"Origin":         "https://accounts.google.com",
		"Cookie":         subset,
		"Cache-Control":  "no-cache",
		"Pragma":         "no-cache",
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Site": "same-origin",
	}
	status, setCookie, _, err := rotatePost(rotatePostURL, headers, []byte(rotate1PSIDTSBody), proxyURL)
	merged, names := cookie, []string(nil)
	if status == 200 || status == 429 {
		merged = mergeSetCookie(cookie, setCookie)
		names = setCookieNames(setCookie)
	}
	if err != nil {
		return merged, names, err
	}
	if status != 200 {
		return cookie, nil, &rotationHTTPError{Status: status, RetryAfter: defaultRotateInterval}
	}
	if cookieValue(merged, "__Secure-1PSIDTS") == "" || cookieValue(merged, "__Secure-1PSIDTS") == cookieValue(cookie, "__Secure-1PSIDTS") {
		return merged, names, fmt.Errorf("RotateCookies HTTP 200 未换发新的 1PSIDTS")
	}
	return merged, names, nil
}

// rotateSIDCC 走浏览器 iframe 那条：GET 轮转页拿会话 id，再 POST [658, id]。
func rotateSIDCC(cookie, proxyURL string) (string, time.Duration, []string, error) {
	id, interval, pageSet, err := fetchRotateParams(cookie, proxyURL)
	cookie = mergeSetCookie(cookie, pageSet)
	names := setCookieNames(pageSet)
	if err != nil {
		return cookie, interval, names, err
	}

	body := fmt.Sprintf(`[%d,"%s"]`, rotateProductID, id)
	headers := rotatePostHeaders()
	headers["Cookie"] = cookie
	status, setCookie, respBody, err := rotatePost(rotatePostURL, headers, []byte(body), proxyURL)
	if status == 200 || status == 429 {
		cookie = mergeSetCookie(cookie, setCookie)
		names = uniqueKeepOrder(append(names, setCookieNames(setCookie)...))
	}
	if err != nil {
		if e, ok := err.(*rotationHTTPError); ok && e.RetryAfter > interval {
			interval = e.RetryAfter
		}
		return cookie, interval, names, err
	}
	if status != 200 {
		return cookie, interval, names, fmt.Errorf("RotateCookies 返回 HTTP %d: %s", status, truncate(string(respBody), 120))
	}
	return cookie, interval, names, nil
}

func uniqueKeepOrder(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// setCookieNames 把 Set-Cookie 头里的名字抽出来去重，只用于日志。
func setCookieNames(headers []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range headers {
		name := h
		if i := strings.Index(name, "="); i > 0 {
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// fetchRotateParams 抓 RotateCookiesPage，取会话标识、服务端指定的间隔，以及这一发
// 自己带回的 Set-Cookie。
func fetchRotateParams(cookie, proxyURL string) (string, time.Duration, []string, error) {
	status, setCookie, body, err := rotateGet(rotatePageURL, cookie, proxyURL)
	if err != nil {
		return "", 0, nil, err
	}
	if status != 200 {
		return "", 0, nil, fmt.Errorf("取轮转页返回 HTTP %d", status)
	}
	m := rotateInitRe.FindSubmatch(body)
	if m == nil {
		// 页面拿到了却没有 init(...)，最可能是 cookie 已失效跳到了登录页。
		return "", defaultRotateInterval, setCookie, fmt.Errorf("轮转页里没有 init(...)（cookie 可能已失效）")
	}
	id := string(m[1])
	interval := defaultRotateInterval
	if sec, e := strconv.ParseFloat(string(m[3]), 64); e == nil && sec >= 60 && sec <= 3600 {
		interval = time.Duration(sec) * time.Second
	}
	return id, interval, setCookie, nil
}

// 下面两组 header 逐项抄自抓包（wireHeaders，不是 headers —— 后者不含 cookie）。
// 抓包里还有 sec-ch-ua-arch / -bitness / -form-factors / -full-version-list /
// -model / -platform-version / -wow64 和 x-browser-* / x-client-data /
// x-chrome-id-consistency-request，那些是 Chrome 自己贴的浏览器身份，我们贴了反而
// 会跟 TLS 指纹对不上，所以不贴。

// rotateGet 取轮转页。它在浏览器里是个 iframe 导航，所以 sec-fetch 那组跟普通
// XHR 完全不同（dest=iframe / mode=navigate / site=same-site），别套用默认值。
func rotateGet(url, cookie, proxyURL string) (int, []string, []byte, error) {
	return rotateDo("GET", url, map[string]string{
		"Cookie":                    cookie,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
		"Referer":                   "https://gemini.google.com/",
		"Sec-Fetch-Dest":            "iframe",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "same-site",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
		"Priority":                  "u=0, i",
	}, nil, proxyURL)
}

// rotatePostHeaders 是 POST /RotateCookies 那一发的完整头，调用方只补 Cookie。
func rotatePostHeaders() map[string]string {
	return map[string]string{
		"Accept":         "*/*",
		"Content-Type":   "application/json",
		"Origin":         "https://accounts.google.com",
		"Referer":        rotatePageURL,
		"Cache-Control":  "no-cache",
		"Pragma":         "no-cache",
		"Priority":       "u=1, i",
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "same-origin",
		"Sec-Fetch-Site": "same-origin",
	}
}

func rotatePost(url string, headers map[string]string, body []byte, proxyURL string) (
	int, []string, []byte, error) {
	return rotateDo("POST", url, headers, body, proxyURL)
}

// rotateDo 走跟正式请求同一个出口：保活从别的 IP 发，等于告诉上游这个会话在两处活动。
//
// 两条传输路径共用同一份 header。以前只有走代理那条调 applyChromeHeaders，直连那条
// 连 User-Agent 都不发 —— 同一个账号在上游看来会因为走没走代理而呈现两种客户端。
func rotateDo(method, url string, headers map[string]string, body []byte, proxyURL string) (
	int, []string, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rotateRequestTimeout)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	merged := map[string]string{
		"User-Agent":      currentBrowserUA(),
		"Accept-Language": "en-US,en;q=0.9",
	}
	if !isFirefoxProfile() {
		merged["Sec-CH-UA"] = `"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="24"`
		merged["Sec-CH-UA-Mobile"] = "?0"
		merged["Sec-CH-UA-Platform"] = `"Windows"`
	}
	for k, v := range headers {
		merged[k] = v
	}
	if proxyURL != "" {
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return 0, nil, nil, err
		}
		for k, v := range merged {
			req.Header.Set(k, v)
		}
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == 429 {
			err = &rotationHTTPError{Status: 429, RetryAfter: rotationRetryAfter(resp.Header.Get("Retry-After"), b, time.Now())}
		}
		return resp.StatusCode, resp.Header.Values("Set-Cookie"), b, err
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range merged {
		req.Header.Set(k, v)
	}
	resp, err := getTLSClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 429 {
		err = &rotationHTTPError{Status: 429, RetryAfter: rotationRetryAfter(resp.Header.Get("Retry-After"), b, time.Now())}
	}
	return resp.StatusCode, resp.Header.Values("Set-Cookie"), b, err
}

// rotateAllAccounts 给池子里每个启用的账号做一次保活，返回下次该等多久。
//
// 失败**不计入健康度**：保活打的是 accounts.google.com，跟对话能不能用是两码事，
// 网络抖一下就把号标成坏的，会让它在挑号时沉底，反而伤可用性。
func rotateAllAccounts() time.Duration {
	next := defaultRotateInterval
	for _, a := range accountList() {
		if a.Status != "enabled" {
			continue
		}
		iv, _, err := rotateAccount(a)
		if err != nil {
			var deferred *rotationDeferredError
			if !errors.As(err, &deferred) {
				logf("[rotate] 账号 #%d 保活失败: %v", a.ID, err)
			}
		}
		if iv > 0 && iv < next {
			next = iv
		}
	}
	return next
}
