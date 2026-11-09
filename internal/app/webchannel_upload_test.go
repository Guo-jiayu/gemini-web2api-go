package app

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type wcRoundTripFunc func(*http.Request) (*http.Response, error)

func (f wcRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The upstream can acknowledge an upload after it has already sent answer
// frames on the separate downlink. Upload acknowledgement must not gate text.
func TestBrowserTextDoesNotWaitForUploadAck(t *testing.T) {
	proxy := "http://delayed-wc-ack.invalid:1234"
	key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
	ack := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(ack) }) }
	defer release()
	uploadStarted, frameRead, gotText := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	uploadStopped := make(chan struct{})
	response := func(body io.ReadCloser) *http.Response {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}
	}
	client := &http.Client{Transport: wcRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Has("$httpHeaders") {
			r := response(io.NopCloser(strings.NewReader("1\n[[0,[\"c\",\"fake-session\"]]]")))
			r.Header.Set("X-HTTP-Session-Id", "fake-gsession")
			return r, nil
		}
		if req.Method == http.MethodPost {
			body, _ := io.ReadAll(req.Body)
			form, _ := url.ParseQuery(string(body))
			if form.Get("count") != "2" || form.Get("ofs") != "0" {
				return nil, fmt.Errorf("unexpected upload")
			}
			close(uploadStarted)
			reader, writer := io.Pipe()
			go func() {
				defer close(uploadStopped)
				defer writer.Close()
				select {
				case <-ack:
					_, _ = io.WriteString(writer, "ok")
				case <-req.Context().Done():
					_ = writer.CloseWithError(req.Context().Err())
				}
			}()
			return response(reader), nil
		}
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			select {
			case <-uploadStarted:
			case <-req.Context().Done():
				return
			}
			_, _ = io.WriteString(writer, "1\n[[1,"+wcContentPayload("0")+"]]")
			close(frameRead)
			_, _ = io.WriteString(writer, "1\n[[2,[[null,null,null,[]]]]]")
		}()
		return response(reader), nil
	})}
	stdlibClientCache.Store(key, client)
	defer stdlibClientCache.Delete(key)
	finished := make(chan error, 1)
	go func() {
		status, _, _, _, err := doWebChannelRequest(wcRequest{
			APIKey: "fake-key", ServerToken: "fake-token", Inner: make([]interface{}, 99), Prompt: "test",
		}, proxy, func(line string) error {
			if len(textsInLine(line)) > 0 {
				select {
				case gotText <- struct{}{}:
				default:
				}
			}
			return nil
		})
		if err == nil && status != 200 {
			err = fmt.Errorf("status %d", status)
		}
		finished <- err
	}()
	select {
	case <-frameRead:
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("downlink frame was not read")
	}
	select {
	case <-gotText:
	case <-time.After(300 * time.Millisecond):
		t.Error("answer arrived on downlink but callback waited for the upload acknowledgement")
	}
	// End-of-stream must cancel the still-pending POST and release its goroutine.
	select {
	case <-uploadStopped:
	case <-time.After(300 * time.Millisecond):
		t.Error("finished downlink did not cancel the pending upload")
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}
}

func TestBrowserTextUploadFailureIsReported(t *testing.T) {
	for _, afterText := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_text=%t", afterText), func(t *testing.T) {
			proxy := fmt.Sprintf("http://wc-upload-failure-%t.invalid:1234", afterText)
			key := fmt.Sprintf("%s|%d", proxy, rtCfg().RequestTimeout)
			started, emitted := make(chan struct{}), make(chan struct{})
			var emitOnce sync.Once
			client := &http.Client{Transport: wcRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				response := func(status int, body io.ReadCloser) *http.Response {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}
				}
				if req.URL.Query().Has("$httpHeaders") {
					r := response(200, io.NopCloser(strings.NewReader("1\n[[0,[\"c\",\"fake-session\"]]]")))
					r.Header.Set("X-HTTP-Session-Id", "fake-gsession")
					return r, nil
				}
				if req.Method == http.MethodPost {
					close(started)
					if afterText {
						select {
						case <-emitted:
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					}
					return response(503, io.NopCloser(strings.NewReader("test unavailable"))), nil
				}
				reader, writer := io.Pipe()
				go func() {
					defer writer.Close()
					select {
					case <-started:
					case <-req.Context().Done():
						return
					}
					if afterText {
						_, _ = io.WriteString(writer, "1\n[[1,"+wcContentPayload("0")+"]]")
					}
					<-req.Context().Done()
					_ = writer.CloseWithError(req.Context().Err())
				}()
				return response(200, reader), nil
			})}
			stdlibClientCache.Store(key, client)
			defer stdlibClientCache.Delete(key)
			finished := make(chan error, 1)
			go func() {
				status, raw, _, _, err := doWebChannelRequest(wcRequest{APIKey: "fake-key", ServerToken: "fake-token", Inner: make([]interface{}, 99)}, proxy, func(line string) error {
					if len(textsInLine(line)) > 0 {
						emitOnce.Do(func() { close(emitted) })
					}
					return nil
				})
				if err == nil || status != 0 || !strings.Contains(err.Error(), "HTTP 503") {
					finished <- fmt.Errorf("lost upload failure: status=%d error=%v", status, err)
					return
				}
				if strings.Contains(string(raw), wcTestText) != afterText {
					finished <- fmt.Errorf("partial downlink was not preserved correctly")
					return
				}
				finished <- nil
			}()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upload failure left the request waiting")
			}
		})
	}
}
