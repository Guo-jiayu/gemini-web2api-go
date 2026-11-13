package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Opt-in export for the separate new-api relaykit consumer check. No network.
func TestNewAPIExportContract(t *testing.T) {
	dir := os.Getenv("WEB2API_CONTRACT_DIR")
	if dir == "" {
		t.Skip("set WEB2API_CONTRACT_DIR to export handler responses")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	withPoolCookie(t)
	stubGeminiCall(t, "", nil, &StreamResult{Artifacts: []MediaArtifact{testImageArtifact(t, 2, 2)}}, nil)
	for _, stream := range []bool{false, true} {
		w := httptest.NewRecorder()
		handleImages(w, httptest.NewRequest("POST", "/v1/images/generations",
			strings.NewReader(fmt.Sprintf(`{"prompt":"cat","stream":%v}`, stream))))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		name := "images.json"
		if stream {
			name = "images.sse"
		}
		write(name, w.Body.Bytes())
	}
	w := httptest.NewRecorder()
	handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-image:generateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"cat"}]}]}`)))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	write("gemini.json", w.Body.Bytes())
	usage := usageOf("hello", "answer", &StreamResult{Reasoning: "thinking through the answer"})
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	write("usage.json", encoded)
	w = httptest.NewRecorder()
	s := newSSEWriter(w, "chatcmpl-contract", 1, "gemini-3.6-flash")
	s.SendContent("answer")
	s.Finish("stop", usage)
	write("chat.sse", w.Body.Bytes())
}

var legacyDataURLRe = regexp.MustCompile(`data:([-\w.+/]+);base64,[A-Za-z0-9+/=]+`)

func TestStripDataURLsMatchesLegacy(t *testing.T) {
	for _, text := range []string{
		"", "plain text 中文", "data:image/png;base64,", "data:;base64,AAAA",
		"![image](data:image/png;base64,AA+/==) tail",
		"data:bad mime;base64,AAAA data:image/x-test_2+png;base64,1234!",
		"data:image/png;base64,! data:image/jpeg;base64,AAAA\nnext",
		"data:image/png;base64,data:image/png;base64,AAAA",
		"data:image/png;base64," + strings.Repeat("Ab9+/=", 1<<18) + ")",
	} {
		want := legacyDataURLRe.ReplaceAllString(text, "data:$1;base64,<omitted>")
		if got := stripDataURLs(text); got != want {
			t.Fatalf("strip mismatch: got %.200q, want %.200q", got, want)
		}
	}
}

func FuzzStripDataURLs(f *testing.F) {
	f.Add("head", "image/png", "AA+/=", "tail")
	f.Fuzz(func(t *testing.T, head, mime, data, tail string) {
		text := head + "data:" + mime + ";base64," + data + tail
		want := legacyDataURLRe.ReplaceAllString(text, "data:$1;base64,<omitted>")
		if stripDataURLs(text) != want {
			t.Fatal("data URL stripping changed semantics")
		}
	})
}

func TestGatewayWrappedErrors(t *testing.T) {
	withPoolCookie(t)
	for _, tc := range []struct {
		err    error
		status int
		retry  string
	}{
		{&RateLimitError{Reason: "concurrent", RetryAfter: 2 * time.Second}, 429, "2"},
		{&ModelDowngradedError{Requested: "a", Reported: "b", RetryAfter: 2 * time.Second}, 429, "2"},
		{&PromptTooLongError{Bytes: 20, Budget: 10}, 400, ""},
		{fmt.Errorf("download failed"), 502, ""},
	} {
		err := fmt.Errorf("generation failed: %w", tc.err)
		w := httptest.NewRecorder()
		writeGeminiCallError(w, err)
		if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retry {
			t.Fatalf("Gemini: %d %v", w.Code, w.Header())
		}
		stubGeminiCall(t, "", nil, nil, err)
		w = httptest.NewRecorder()
		handleImages(w, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"prompt":"cat"}`)))
		if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retry {
			t.Fatalf("Images: %d %v", w.Code, w.Header())
		}
		w = httptest.NewRecorder()
		s := &geminiStream{w: w, flusher: w, sse: true}
		s.text("partial")
		s.fail(err)
		chunks := sseChunks(t, w.Body.String())
		last := chunks[len(chunks)-1]["error"].(map[string]any)
		if int(last["code"].(float64)) != tc.status || last["status"] != geminiStatus(tc.status) {
			t.Fatalf("Gemini stream lost error category: %v", last)
		}
	}
}

func BenchmarkStripImageDataURL(b *testing.B) {
	text := "![image](data:image/png;base64," + strings.Repeat("Ab9+", 1<<20) + ")"
	for _, legacy := range []bool{true, false} {
		b.Run(fmt.Sprintf("legacy=%v", legacy), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				if legacy {
					_ = legacyDataURLRe.ReplaceAllString(text, "data:$1;base64,<omitted>")
				} else {
					_ = stripDataURLs(text)
				}
			}
		})
	}
}

// Same native response bytes, including base64 encoding and JSON serialization.
// The legacy path first constructed and stripped a discarded Markdown copy.
func BenchmarkNativeImagePreparation(b *testing.B) {
	arts := []MediaArtifact{{Mime: "image/png", Data: bytes.Repeat([]byte{1, 2, 3}, 1<<20)}}
	for _, legacy := range []bool{true, false} {
		b.Run(fmt.Sprintf("legacy=%v", legacy), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if legacy {
					text := appendArtifactMarkdown("generated", arts)
					_ = legacyDataURLRe.ReplaceAllString(text, "data:$1;base64,<omitted>")
				}
				_, _ = json.Marshal(artifactParts(arts))
			}
		})
	}
}
