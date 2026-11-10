package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 哨兵必须是这串 JSPB，不能走 json.Marshal：后者会把 000 收成 0，服务端不认。
func TestRotate1PSIDTSBodyIsJSPBSentinel(t *testing.T) {
	const want = `[000,"-0000000000000000000"]`
	if rotate1PSIDTSBody != want {
		t.Fatalf("payload = %q，期望 %q", rotate1PSIDTSBody, want)
	}
	b, err := json.Marshal([]interface{}{0, "-0000000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == rotate1PSIDTSBody {
		t.Fatal("json.Marshal 的结果碰巧跟哨兵一样了，这个测试该改")
	}
}

func TestCookieSubsetKeepsOnlyNamed(t *testing.T) {
	in := "SID=a; __Secure-1PSID=psid; SAPISID=sap; __Secure-1PSIDTS=ts; NID=n"
	got := cookieSubset(in, []string{"__Secure-1PSID", "__Secure-1PSIDTS"})
	if got != "__Secure-1PSID=psid; __Secure-1PSIDTS=ts" {
		t.Fatalf("got %q", got)
	}
}

func TestCookieSubsetSkipsEmptyAndDup(t *testing.T) {
	in := "__Secure-1PSID=psid; __Secure-1PSIDTS=; __Secure-1PSID=other"
	got := cookieSubset(in, rotate1PSIDTSCookies)
	if got != "__Secure-1PSID=psid" {
		t.Fatalf("got %q", got)
	}
}

func TestCookieValue(t *testing.T) {
	in := "SID=a; __Secure-1PSID=psid==; SAPISID=sap"
	if got := cookieValue(in, "__Secure-1PSID"); got != "psid==" {
		t.Fatalf("got %q", got)
	}
	if got := cookieValue(in, "nope"); got != "" {
		t.Fatalf("missing should be empty, got %q", got)
	}
}

func TestMergeSetCookieRefreshes1PSIDTS(t *testing.T) {
	base := "SID=a; __Secure-1PSID=psid; __Secure-1PSIDTS=oldts; SAPISID=sap"
	got := mergeSetCookie(base, []string{
		"__Secure-1PSIDTS=newts; Domain=.google.com; Secure; HttpOnly",
		"__Secure-3PSIDTS=new3; Domain=.google.com; Secure; HttpOnly",
	})
	if !strings.Contains(got, "__Secure-1PSIDTS=newts") {
		t.Errorf("1PSIDTS 没刷新: %s", got)
	}
	if strings.Contains(got, "oldts") {
		t.Errorf("旧 1PSIDTS 还在: %s", got)
	}
	if !strings.Contains(got, "__Secure-3PSIDTS=new3") {
		t.Errorf("3PSIDTS 没追加: %s", got)
	}
	for _, want := range []string{"SID=a", "__Secure-1PSID=psid", "SAPISID=sap"} {
		if !strings.Contains(got, want) {
			t.Errorf("丢了 %q: %s", want, got)
		}
	}
}

func TestUniqueKeepOrder(t *testing.T) {
	got := uniqueKeepOrder([]string{"SIDCC", "__Secure-1PSIDTS", "SIDCC", "", "__Secure-1PSIDTS"})
	if strings.Join(got, ",") != "SIDCC,__Secure-1PSIDTS" {
		t.Fatalf("got %v", got)
	}
}

func TestMin1PSIDTSInterval(t *testing.T) {
	if min1PSIDTSInterval < 60*time.Second {
		t.Fatalf("地板太短会 429: %s", min1PSIDTSInterval)
	}
}
