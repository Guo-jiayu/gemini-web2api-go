package app

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// Gemini 在带登录 cookie 时要求 batchexecute 请求多带一个表单字段 at（XSRF token），
// 不带就直接 400，响应体形如 [["er",...,400,...,[{"48448350":["xsrf", ...]}]]]。
// 匿名请求不需要它，所以这个坑一直没暴露——一挂上有效 cookie，所有请求立刻全挂。
//
// token 来自 /app 页面 HTML 里的 "SNlM0e":"<token>:<毫秒时间戳>"，跟 cookie 会话
// 绑定，所以按 cookie 分别缓存。实测同一 token 可以复用，过期后服务端还是回 xsrf
// 错误，调用方拿到这个错要 invalidate 再取一次。
var (
	xsrfMu      sync.Mutex
	xsrfCache   = map[string]xsrfEntry{}
	xsrfPending = map[string]*xsrfCall{}
	xsrfHits    atomic.Uint64
	xsrfMisses  atomic.Uint64
	xsrfShared  atomic.Uint64
)

type xsrfCall struct {
	done        chan struct{}
	entry       xsrfEntry
	err         error
	invalidated bool
}

type xsrfEntry struct {
	token       string
	pushID      string // 上传文件用的 Push-ID 头
	pctx        string // 上传文件用的 X-Client-Pctx 头
	serverToken string // 网页文本连接使用的账号会话令牌，按会话和出口隔离
	apiKey      string // 同页取得，避免首次文本请求再抓一次匿名页面
	fetched     time.Time
}

// 页面上的 token 没写明有效期，取个保守值定期重取。
const xsrfTTL = 20 * time.Minute

var snlm0eRe = regexp.MustCompile(`"SNlM0e":"([^"]{10,200})"`)

// 上传要的两个页面参数，跟 XSRF token 同页取，省一次页面请求。
var pushIDRe = regexp.MustCompile(`"qKIAYe":"([^"]{4,400})"`)

var pctxRe = regexp.MustCompile(`"Ylro7b":"([^"]{4,400})"`)

var serverTokenRe = regexp.MustCompile(`"PI9WOb":("(?:[^"\\]|\\.)*")`)

func appServerToken(body []byte) string {
	m := serverTokenRe.FindSubmatch(body)
	if len(m) != 2 {
		return ""
	}
	var token string
	if json.Unmarshal(m[1], &token) != nil {
		return ""
	}
	return token
}

func getBrowserSession(cookie string, authuser int, proxyURL string) (xsrfEntry, error) {
	if cookie == "" {
		return xsrfEntry{}, nil
	}
	return cachedAppTokens(cookie, authuser, proxyURL, fetchAppTokens)
}

// cookieKey 用 cookie 的短摘要当缓存键，避免把整串凭证塞进 map key。
func cookieKey(cookie string) string {
	sum := sha1.Sum([]byte(cookie))
	return hex.EncodeToString(sum[:8])
}

// Only stable session credentials participate in this key. Google rotates SIDCC /
// SIDTS on ordinary responses; those changes must not trigger another /app GET.
// A new login, different account index, or changed signing credential is isolated.
func xsrfKey(cookie string, authuser int) string {
	var credentials []string
	for _, name := range []string{"SID", "__Secure-1PSID", "__Secure-3PSID",
		"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		if value := cookieValue(cookie, name); value != "" {
			credentials = append(credentials, name+"="+value)
		}
	}
	identity := strings.Join(credentials, "; ")
	if identity == "" {
		identity = cookie
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:]) + "/" + strconv.Itoa(authuser)
}

// geminiBase 是某个账号序号对应的 gemini.google.com 前缀。
//
// 浏览器多号登录时 cookie 只有一份，选哪个号看请求路径：/u/N/ 落到第 N 个号，
// 裸路径落到默认号（等同 /u/0/）。实测 StreamGenerate 和 batchexecute 都认路径，
// X-Goog-AuthUser 头被忽略（裸路径 + 头写 1，会话照样落在默认号）。
func geminiBase(authuser int) string {
	if authuser > 0 {
		return "https://gemini.google.com/u/" + strconv.Itoa(authuser)
	}
	return "https://gemini.google.com"
}

// invalidateXSRF 丢掉某个 cookie 的缓存 token，下次取会重新抓页面。
// 同一串 cookie 的各个账号序号一起作废：cookie 一变（轮转）或 token 被拒，
// 哪个号的缓存都不该再信。
func invalidateXSRF(cookie string) {
	if cookie == "" {
		return
	}
	xsrfMu.Lock()
	for n := 0; n <= maxAuthUser; n++ {
		key := xsrfKey(cookie, n)
		for k := range xsrfCache {
			if k == key || strings.HasPrefix(k, key+"/") {
				delete(xsrfCache, k)
			}
		}
		// Detach an in-flight fetch too: it must not repopulate an invalidated
		// session. Existing waiters will retry against the new generation.
		for k, call := range xsrfPending {
			if k == key || strings.HasPrefix(k, key+"/") {
				call.invalidated = true
				delete(xsrfPending, k)
			}
		}
	}
	xsrfMu.Unlock()
}

// getXSRF 取该 cookie（第 authuser 个号）对应的 XSRF token；命中缓存且没过期就直接返回。
// cookie 为空（匿名）时返回空串——匿名请求不需要这个字段。
func getXSRF(cookie string, authuser int, proxyURL string) (string, error) {
	if cookie == "" {
		return "", nil
	}
	e, err := cachedAppTokens(cookie, authuser, proxyURL, fetchAppTokens)
	return e.token, err
}

// getUploadTokens 取上传要用的 Push-ID / X-Client-Pctx，跟 XSRF token 同一份缓存。
func getUploadTokens(cookie string, authuser int, proxyURL string) (pushID, pctx string, err error) {
	e, err := cachedAppTokens(cookie, authuser, proxyURL, fetchAppTokens)
	return e.pushID, e.pctx, err
}

// Cache and coalesce both token consumers. Expired entries are evicted during
// misses so a server that imports many accounts does not retain them forever.
func cachedAppTokens(cookie string, authuser int, proxyURL string,
	fetch func(string, int, string) (xsrfEntry, error)) (xsrfEntry, error) {
	start := time.Now()
	key := xsrfCacheKey(cookie, authuser, proxyURL)
	for {
		xsrfMu.Lock()
		if e, ok := xsrfCache[key]; ok && time.Since(e.fetched) < xsrfTTL {
			xsrfMu.Unlock()
			xsrfHits.Add(1)
			return e, nil
		}
		if call := xsrfPending[key]; call != nil {
			xsrfMu.Unlock()
			xsrfShared.Add(1)
			<-call.done
			xsrfMu.Lock()
			current := !call.invalidated
			xsrfMu.Unlock()
			if current {
				return call.entry, call.err
			}
			// A completed successful fetch is now in the cache. A detached fetch
			// was invalidated; loop instead of trusting its stale result.
			continue
		}
		for k, e := range xsrfCache {
			if time.Since(e.fetched) >= xsrfTTL {
				delete(xsrfCache, k)
			}
		}
		call := &xsrfCall{done: make(chan struct{})}
		xsrfPending[key] = call
		xsrfMu.Unlock()
		xsrfMisses.Add(1)
		e, err := fetch(cookie, authuser, proxyURL)
		xsrfMu.Lock()
		current := xsrfPending[key] == call
		if current {
			delete(xsrfPending, key)
			if err == nil {
				xsrfCache[key] = e
			}
		}
		call.entry, call.err = e, err
		close(call.done)
		xsrfMu.Unlock()
		if !current {
			continue
		}
		if rtCfg().LogRequests {
			logf("[xsrf] cache=miss authuser=%d fetch_ms=%d ok=%t hits=%d misses=%d shared=%d", authuser, time.Since(start).Milliseconds(), err == nil, xsrfHits.Load(), xsrfMisses.Load(), xsrfShared.Load())
		}
		return e, err
	}
}

func xsrfCacheKey(cookie string, authuser int, proxyURL string) string {
	key := xsrfKey(cookie, authuser)
	if proxyURL != "" {
		key += "/" + cookieKey(proxyURL)
	}
	return key
}

// fetchAppPage 抓 gemini.google.com/app 的 HTML。
// 走跟主请求相同的出口：配了代理走 stdlib，没配走 tls-client，
// 免得页面里取到的 token 和后续请求来自两个不同 IP。
// cookie 传空串就是匿名抓（页面照样返回，只是没有登录态字段）。
// authuser>0 抓 /u/N/app，页面里的 token 才是第 N 个号的。
func fetchAppPage(cookie string, authuser int, proxyURL string) ([]byte, error) {
	// Session setup must not inherit the multi-minute generation timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pageURL := geminiBase(authuser) + "/app"
	headers := map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.9",
	}
	if cookie != "" {
		headers["Cookie"] = cookie
	}

	var body []byte
	if proxyURL != "" {
		req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
		if err != nil {
			return nil, err
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, newAppPageStatusError(resp.StatusCode, resp.Header.Get("Location"))
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	} else {
		req, err := fhttp.NewRequest("GET", pageURL, nil)
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := getTLSClient().Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, newAppPageStatusError(resp.StatusCode, resp.Header.Get("Location"))
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// Preload enabled sessions after startup without blocking the HTTP listener or
// generating any model output. Reuses the normal egress limiter and singleflight.
func warmEnabledAppTokens() {
	// Large pools must not spend their entire RPM/RPH budget on startup work.
	// Warm at most four accounts, at most one per egress; the rest load lazily.
	for _, account := range warmupCandidates(accountList(), listProxies(), 4) {
		current := accountByID(account.ID)
		if current == nil || current.Status != "enabled" {
			continue
		}
		account = *current
		// Warmup is read-only with respect to account routing. Let the real
		// request bind a new account; never steal another proxy's slot or
		// silently rebind an existing account as a background side effect.
		proxies := listProxies()
		p, routeOK := warmupRoute(account.ProxyID, proxies)
		if !routeOK {
			continue
		}
		xsrfMu.Lock()
		e, cached := xsrfCache[xsrfCacheKey(account.Cookie, account.AuthUser, p.URL)]
		xsrfMu.Unlock()
		if cached && time.Since(e.fetched) < xsrfTTL {
			continue
		}
		if ok, _ := trySlotAcquire(p.ID); !ok {
			continue
		}
		_, err := getXSRF(account.Cookie, account.AuthUser, p.URL)
		releaseSlot(p.ID)
		logf("[xsrf] warmup account=%d proxy=%d ok=%t", account.ID, p.ID, err == nil)
	}
}

func warmupCandidates(accounts []CookieAccount, proxies []Proxy, limit int) []CookieAccount {
	var out []CookieAccount
	seen := map[int64]bool{}
	for _, a := range accounts {
		if len(out) >= limit {
			break
		}
		if a.Status != "enabled" || seen[a.ProxyID] {
			continue
		}
		if _, ok := warmupRoute(a.ProxyID, proxies); !ok {
			continue
		}
		seen[a.ProxyID] = true
		out = append(out, a)
	}
	return out
}

func warmupRoute(proxyID int64, proxies []Proxy) (Proxy, bool) {
	if len(proxies) == 0 {
		return Proxy{}, proxyID == 0
	}
	for _, p := range proxies {
		if p.ID == proxyID && proxyUsable(p, time.Now().Unix(), rtCfg().ProxyCooldownMin) {
			return p, true
		}
	}
	return Proxy{}, false
}

// fetchAppTokens 抓一次 /app 页面，共用 XSRF、上传参数和网页文本会话凭据。
func fetchAppTokens(cookie string, authuser int, proxyURL string) (xsrfEntry, error) {
	body, err := fetchAppPage(cookie, authuser, proxyURL)
	if err != nil {
		return xsrfEntry{}, err
	}
	e := xsrfEntry{fetched: time.Now(), serverToken: appServerToken(body)}
	if m := wcKeyRe.FindSubmatch(body); m != nil {
		e.apiKey = string(m[1])
	}
	if m := snlm0eRe.FindSubmatch(body); m != nil {
		e.token = string(m[1])
	} else if cookie != "" {
		// 带 cookie 却拿不到 token = cookie 已失效被当成匿名。匿名本就没这字段，不算错。
		return xsrfEntry{}, fmt.Errorf("no SNlM0e in page (cookie expired or not signed in)")
	}
	if p := pushIDRe.FindSubmatch(body); p != nil {
		e.pushID = string(p[1])
	}
	if p := pctxRe.FindSubmatch(body); p != nil {
		e.pctx = string(p[1])
	}
	return e, nil
}

// isXSRFError 判断上游 400 是不是 XSRF token 的问题。
// 响应体形如：[["er",null,...,400,...,[{"48448350":["xsrf","<新token>",...]}]]]
func isXSRFError(raw string) bool {
	return strings.Contains(raw, `"xsrf"`)
}

// appPageStatusError 是抓 /app 拿到非 200 时的错误，带上重定向目标。
//
// 两个 client 都不跟随重定向，所以 302 本身说明不了原因：跳 accounts.google.com
// 是登录态没了，跳 www.google.com/sorry/ 是出口被 Google 拦，跟 cookie 无关。
// 只留 host+path：/sorry 的 query 里有长串 continue/q 参数，进日志和面板没用。
// 文本仍以 "fetch /app: HTTP <code>" 开头，按子串判 "HTTP 302" 的地方不受影响。
type appPageStatusError struct {
	Status   int
	Location string // host+path，没有 Location 头时为空
}

func newAppPageStatusError(status int, location string) *appPageStatusError {
	e := &appPageStatusError{Status: status}
	if location != "" {
		if u, err := url.Parse(location); err == nil {
			e.Location = u.Host + u.Path
		} else {
			e.Location = location
		}
	}
	return e
}

func (e *appPageStatusError) Error() string {
	if e.Location != "" {
		return fmt.Sprintf("fetch /app: HTTP %d -> %s", e.Status, e.Location)
	}
	return fmt.Sprintf("fetch /app: HTTP %d", e.Status)
}
