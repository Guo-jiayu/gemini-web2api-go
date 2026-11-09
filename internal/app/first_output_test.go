package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func firstOutputTestLine(reasoning bool) string {
	if !reasoning {
		return wcClassify(json.RawMessage(wcContentPayload("0"))).Line
	}
	part := make([]interface{}, 38)
	part[0], part[1], part[37] = "rc_test", []string{""}, []interface{}{[]string{"Thinking through this test carefully."}}
	inner := make([]interface{}, 45)
	inner[4] = []interface{}{part}
	b, _ := json.Marshal(inner)
	b, _ = json.Marshal([]interface{}{[]interface{}{"wrb.fr", nil, string(b)}})
	return string(b)
}

func TestFirstOutputGuard(t *testing.T) {
	for _, reasoning := range []bool{false, true} {
		t.Run(fmt.Sprint(reasoning), func(t *testing.T) {
			ctx, line, done := firstOutputGuard(context.Background(), 80*time.Millisecond, nil)
			defer done()
			if err := line(firstOutputTestLine(reasoning)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				t.Fatal("first output did not stop timer")
			case <-time.After(160 * time.Millisecond):
			}
		})
	}
	ctx, line, done := firstOutputGuard(context.Background(), 30*time.Millisecond, func(string) error { return nil })
	defer done()
	if err := line(`[["noop"]]`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("metadata suppressed timeout")
	}
	if !errors.Is(context.Cause(ctx), errFirstOutputTimeout) {
		t.Fatal(context.Cause(ctx))
	}
	if !errors.Is(line(firstOutputTestLine(false)), errFirstOutputTimeout) {
		t.Fatal("late output was accepted")
	}
}

// Exercise real transport cancellation, including the parallel WebChannel upload.
func TestFirstOutputTransportDeadline(t *testing.T) {
	for _, wc := range []bool{false, true} {
		for _, output := range []bool{false, true} {
			t.Run(fmt.Sprintf("wc=%t/output=%t", wc, output), func(t *testing.T) {
				proxy := fmt.Sprintf("http://first-output-%t-%t.invalid", wc, output)
				key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
				client := &http.Client{Transport: wcRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					response := func(body io.ReadCloser) *http.Response {
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}
					}
					if wc && req.URL.Query().Has("$httpHeaders") {
						r := response(io.NopCloser(strings.NewReader("1\n[[0,[\"c\",\"session\"]]]")))
						r.Header.Set("X-HTTP-Session-Id", "gsession")
						return r, nil
					}
					if wc && req.Method == http.MethodPost {
						return response(io.NopCloser(strings.NewReader("ok"))), nil
					}
					reader, writer := io.Pipe()
					go func() {
						defer writer.Close()
						if output {
							line := firstOutputTestLine(false) + "\n"
							if wc {
								line = "1\n[[1," + wcContentPayload("0") + "]]"
							}
							_, _ = io.WriteString(writer, line)
						}
						select {
						case <-req.Context().Done():
							_ = writer.CloseWithError(req.Context().Err())
						case <-time.After(200 * time.Millisecond):
							if wc {
								_, _ = io.WriteString(writer, "1\n[[2,[[null,null,null,[]]]]]")
							}
						}
					}()
					return response(reader), nil
				})}
				stdlibClientCache.Store(key, client)
				defer stdlibClientCache.Delete(key)
				var err error
				if wc {
					_, _, _, _, err = doWebChannelRequest(wcRequest{APIKey: "test", ServerToken: "test", Inner: make([]interface{}, 99)}, proxy, nil, 80*time.Millisecond)
				} else {
					_, _, _, _, err = doGeminiRequest("https://example.invalid/generate", "", nil, proxy, nil, 80*time.Millisecond)
				}
				if output && err != nil {
					t.Fatal(err)
				}
				if !output && !errors.Is(err, errFirstOutputTimeout) {
					t.Fatalf("expected first output timeout: %v", err)
				}
			})
		}
	}
}

func TestFirstOutputRetriesExhausted(t *testing.T) {
	const proxy = "http://first-output-retry.invalid"
	withTestProxySlot(t, Proxy{ID: 99900911, URL: proxy, Enabled: true}, func(c *RuntimeConfig) {
		c.RetryAttempts, c.FirstOutputTimeout, c.RequestTimeout = 4, 1, 10
		c.AnonFirst = true
	})
	key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
	var attempts atomic.Int32
	stdlibClientCache.Store(key, &http.Client{Transport: wcRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})})
	defer stdlibClientCache.Delete(key)
	_, err := streamGenerate("test", "test", ModelConfig{}, nil, nil)
	if !errors.Is(err, errFirstOutputTimeout) || attempts.Load() != 4 {
		t.Fatalf("attempts=%d err=%v", attempts.Load(), err)
	}
	if firstOutputWait(ModelConfig{Tool: toolImage}) != 0 {
		t.Fatal("image generation inherits text deadline")
	}
}

func TestFirstOutputPartialStreamDoesNotRetry(t *testing.T) {
	const proxy = "http://first-output-partial.invalid"
	withTestProxySlot(t, Proxy{ID: 99900912, URL: proxy, Enabled: true}, func(c *RuntimeConfig) {
		c.RetryAttempts, c.FirstOutputTimeout, c.RequestTimeout = 4, 1, 10
		c.AnonFirst = true
	})
	key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
	var attempts atomic.Int32
	stdlibClientCache.Store(key, &http.Client{Transport: wcRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		reader, writer := io.Pipe()
		go func() {
			_, _ = io.WriteString(writer, firstOutputTestLine(false)+"\n")
			_ = writer.CloseWithError(errors.New("test interrupted stream"))
		}()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, nil
	})})
	defer stdlibClientCache.Delete(key)
	var output strings.Builder
	_, err := streamGenerate("test", "test", ModelConfig{}, func(s string) { output.WriteString(s) }, nil)
	if err == nil || attempts.Load() != 1 || output.Len() == 0 {
		t.Fatalf("attempts=%d output=%q err=%v", attempts.Load(), output.String(), err)
	}
}
