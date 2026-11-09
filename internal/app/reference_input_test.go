package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A malformed attachment must fail validation before reaching upstream. If the
// Responses conversion flattens content to text, the image error disappears.
func TestResponsesDoesNotDropReferenceImages(t *testing.T) {
	withPoolCookie(t)
	// Block upstream locally even when this regression is reintroduced.
	withTestProxySlot(t, Proxy{ID: 99901001, URL: "http://127.0.0.1:1", Enabled: true,
		FailCount: proxyFailThreshold, LastUsed: time.Now().Unix()}, nil)
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]interface{}{
			"model": "gemini-image", "stream": stream,
			"input": []interface{}{map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "edit the attached image"},
				map[string]interface{}{"type": "input_image", "image_url": "data:image/png;base64,invalid!"},
			}}},
		})
		w := httptest.NewRecorder()
		handleResponses(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "bad base64") {
			t.Errorf("stream=%v: attachment must be validated, got %d %s", stream, w.Code, w.Body)
		}
	}
}

func TestMissingReferenceSourceIsRejected(t *testing.T) {
	for _, part := range []map[string]interface{}{
		{"type": "image_url", "image_url": map[string]interface{}{}},
		{"type": "input_image", "file_id": "file_not_supported"},
		{"type": "input_image", "image_url": " "},
	} {
		_, err := collectImages([]map[string]interface{}{{"role": "user", "content": []interface{}{part}}}, "")
		if err == nil || !strings.Contains(err.Error(), "missing source") {
			t.Errorf("attachment was silently ignored: %v, err=%v", part, err)
		}
	}
}
