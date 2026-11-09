package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestBrowserTextBatchKeepsBothOrderedMessages(t *testing.T) {
	metadata := []interface{}{[]interface{}{[]interface{}{1, "model", "en", "unique-id"}}}
	prompt := []interface{}{nil, []interface{}{1, nil, nil, nil, nil, nil, nil, nil, []interface{}{"中文 & + = newline\nquestion"}}}
	encoded, err := wcMessageForm(0, metadata, prompt)
	if err != nil {
		t.Fatal(err)
	}
	form, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("count") != "2" || form.Get("ofs") != "0" {
		t.Fatal("metadata/prompt were not submitted together at offset 0")
	}
	for i, value := range []interface{}{metadata, prompt} {
		want, _ := json.Marshal(value)
		key := []string{"req0___data__", "req1___data__"}[i]
		if form.Get(key) != string(want) {
			t.Fatalf("message %d changed during encoding", i)
		}
	}
	closeForm, err := wcMessageForm(2, []interface{}{nil, nil, nil, []interface{}{1}})
	if err != nil {
		t.Fatal(err)
	}
	closing, _ := url.ParseQuery(closeForm)
	if closing.Get("count") != "1" || closing.Get("ofs") != "2" {
		t.Fatal("closing offset included the wrong number of messages")
	}
}

func TestBrowserTokenDecodesPageJSON(t *testing.T) {
	for _, tc := range []struct{ page, want string }{
		{`<script>window.WIZ_global_data={"PI9WOb":"session\u003d\u003d","SNlM0e":"other"};</script>`, "session=="},
		{`{"PI9WOb":"one\\two\"three"}`, "one\\two\"three"},
		{`{"SNlM0e":"xsrf-only"}`, ""},
		{`{"PI9WOb":null}`, ""},
	} {
		if got := appServerToken([]byte(tc.page)); got != tc.want {
			t.Errorf("decoded %q, want %q", got, tc.want)
		}
	}
}

func TestBrowserTextRoutePreservesMediaAndContinuation(t *testing.T) {
	c := RuntimeConfig{TextWebChannel: true}
	for name, model := range Models {
		want := model.Tool == 0
		if got := useWebChannelForModel(c, model); got != want {
			t.Errorf("%s: browser route=%t want=%t", name, got, want)
		}
		model.Cont = &ConvRef{CID: "existing-conversation"}
		if useWebChannelForModel(c, model) {
			t.Errorf("%s: rerouted a continuation", name)
		}
	}
	if useWebChannelForModel(RuntimeConfig{}, Models["gemini-3.1-pro"]) {
		t.Fatal("changed default route")
	}
	if !useWebChannelForModel(RuntimeConfig{WebChannel: true}, Models["gemini-image"]) {
		t.Fatal("changed existing explicit WebChannel behavior")
	}
}

func TestBrowserSessionHeaderKeepsAccountAndThinking(t *testing.T) {
	r := wcRequest{HexID: hexArgon4, Mode: 8, Think: thinkingExtended, Cookie: "SID=fake", SAPISID: "fake-signing", AuthUser: 3, ServerToken: "fake-server-token"}
	h := wcHTTPHeaders("fake-key", r)
	if !strings.Contains(h, "X-Server-Token:fake-server-token\r\n") || !strings.Contains(h, "X-Goog-AuthUser:3\r\n") {
		t.Fatal("browser session/account missing")
	}
	if strings.Contains(h, "Authorization:") {
		t.Fatal("mixed legacy and browser session auth")
	}
	foundModel := false
	for _, line := range strings.Split(h, "\r\n") {
		if strings.HasPrefix(line, "x-goog-ext-525001261-bin:") {
			foundModel = true
			b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "x-goog-ext-525001261-bin:"))
			if err != nil || !bytes.Contains(b, []byte(hexArgon4)) || !bytes.Contains(b, []byte{0x78, 8}) || !bytes.Contains(b, []byte{0x80, 1, 2}) {
				t.Fatal("changed selected model/thinking tier")
			}
		}
	}
	if !foundModel {
		t.Fatal("missing model header")
	}
	r.ServerToken = ""
	if !strings.Contains(wcHTTPHeaders("fake-key", r), "Authorization:SAPISIDHASH ") {
		t.Fatal("lost legacy fallback")
	}
}

func TestBrowserTextMetadataDoesNotMutateSharedInput(t *testing.T) {
	inner := make([]interface{}, 97)
	inner[2] = []interface{}{"", ""}
	inner[30] = []interface{}{4}
	inner[79] = 8
	inner[80] = 2
	inner[91] = 0
	inner[96] = 0
	got := wcBrowserTextInner(inner)
	if len(got) != 99 || got[79] != 8 || got[80] != 2 {
		t.Fatal("changed model or thinking")
	}
	if inner[91] != 0 || inner[96] != 0 || len(inner[30].([]interface{})) != 1 || inner[2] == nil {
		t.Fatal("mutated caller metadata")
	}
	got[30].([]interface{})[0] = 99
	if inner[30].([]interface{})[0] != 4 {
		t.Fatal("shared capability slice")
	}
}

func TestWebChannelTransportErrorRedactsHandshakeCredentials(t *testing.T) {
	original := &url.Error{Op: "Post", URL: wcEndpoint + "?$httpHeaders=X-Server-Token%3ASECRET&key=PRIVATE", Err: context.DeadlineExceeded}
	redacted := redactWCError(original)
	if strings.Contains(redacted.Error(), "SECRET") || strings.Contains(redacted.Error(), "PRIVATE") {
		t.Fatal("credential leaked")
	}
	if !errors.Is(redacted, context.DeadlineExceeded) {
		t.Fatal("lost timeout identity")
	}
	if !strings.Contains(original.URL, "SECRET") {
		t.Fatal("mutated caller-owned error")
	}
}
