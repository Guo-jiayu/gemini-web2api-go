package app

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Contract shapes are from QQhuxuhui/new-api at 8faf1d45, relaykit/dto.
func TestNewAPIImagesResponse(t *testing.T) {
	withPoolCookie(t)
	art := testImageArtifact(t, 2, 2)
	for _, format := range []string{"b64_json", "url"} {
		for _, stream := range []bool{false, true} {
			t.Run(format+map[bool]string{true: "/stream", false: "/json"}[stream], func(t *testing.T) {
				seen := stubGeminiCall(t, "generated", nil, &StreamResult{Artifacts: []MediaArtifact{art}}, nil)
				body, _ := json.Marshal(map[string]any{"prompt": "draw a cat", "model": "gemini-image", "n": 1,
					"response_format": format, "stream": stream, "extra_body": map[string]any{"google": map[string]any{
						"image_config": map[string]any{"aspect_ratio": "16:9", "image_size": "2K"}}}})
				w := httptest.NewRecorder()
				handleImages(w, httptest.NewRequest("POST", "/v1/images/generations", bytes.NewReader(body)))
				if w.Code != 200 {
					t.Fatalf("%d: %s", w.Code, w.Body)
				}
				if seen.modelCfg.Image.AspectWidth != 16 || seen.modelCfg.Image.ResolutionK != 2 || !strings.Contains(seen.prompt, "draw a cat") {
					t.Fatalf("lost upstream parameters: %+v", seen)
				}
				var payload map[string]any
				if stream {
					chunks := newAPIChunks(t, w.Body.String())
					if len(chunks) != 1 || chunks[0]["type"] != "image_generation.completed" || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
						t.Fatalf("invalid image SSE: %s", w.Body)
					}
					payload = chunks[0]
				} else {
					var response struct {
						Created int64            `json:"created"`
						Data    []map[string]any `json:"data"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Created == 0 || len(response.Data) != 1 {
						t.Fatalf("invalid Images JSON: %s", w.Body)
					}
					payload = response.Data[0]
				}
				encoded, _ := payload[format].(string)
				if format == "url" {
					if !strings.HasPrefix(encoded, "data:image/png;base64,") {
						t.Fatalf("invalid image URL: %s", encoded)
					}
					encoded = strings.TrimPrefix(encoded, "data:image/png;base64,")
				}
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || !bytes.Equal(decoded, art.Data) {
					t.Fatal("image payload changed or was encoded twice")
				}
			})
		}
	}
}

func TestNewAPIImageEdits(t *testing.T) {
	withPoolCookie(t)
	art := testImageArtifact(t, 2, 2)
	src := "data:image/png;base64," + base64.StdEncoding.EncodeToString(art.Data)
	for _, field := range []string{"image", "images"} {
		for _, ref := range []any{src, map[string]any{"image_url": src}, []any{map[string]any{"image_url": src}, src}} {
			seen := stubGeminiCall(t, "edited", nil, &StreamResult{Artifacts: []MediaArtifact{art}}, nil)
			body, _ := json.Marshal(map[string]any{"prompt": "make it blue", field: ref})
			w := httptest.NewRecorder()
			handleImages(w, httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body)))
			want := 1
			if _, array := ref.([]any); array {
				want = 2
			}
			if w.Code != 200 || len(seen.images) != want || !bytes.Equal(seen.images[0].Data, art.Data) {
				t.Fatalf("JSON edits dropped reference: %d %s", w.Code, w.Body)
			}
		}
	}
	seen := stubGeminiCall(t, "edited", nil, &StreamResult{Artifacts: []MediaArtifact{art}}, nil)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "make it blue")
	_ = mw.WriteField("n", "1")
	_ = mw.WriteField("stream", "true")
	for range 2 {
		f, err := mw.CreateFormFile("image[]", "ref.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(art.Data)
	}
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/v1/images/edits", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	handleImages(w, r)
	if w.Code != 200 || len(seen.images) != 2 || !strings.Contains(w.Body.String(), "image_edit.completed") {
		t.Fatalf("multipart edits failed: %d %s", w.Code, w.Body)
	}
}

func TestNewAPIImageValidationAndErrors(t *testing.T) {
	withPoolCookie(t)
	saved := runGeminiCall
	runGeminiCall = func(*geminiCall, func(string), func(string)) (string, []ToolCall, *StreamResult, error) {
		t.Fatal("invalid request reached upstream")
		return "", nil, nil, nil
	}
	t.Cleanup(func() { runGeminiCall = saved })
	for _, tc := range []struct{ path, body string }{
		{"generations", `null`}, {"generations", `{} {}`},
		{"generations", `{"prompt":""}`}, {"generations", `{"prompt":42}`},
		{"generations", `{"prompt":"cat","n":2}`}, {"generations", `{"prompt":"cat","n":0}`},
		{"generations", `{"prompt":"cat","n":-1}`}, {"generations", `{"prompt":"cat","n":1.5}`},
		{"generations", `{"prompt":"cat","n":"1"}`}, {"generations", `{"prompt":"cat","stream":"true"}`},
		{"generations", `{"prompt":"cat","response_format":"markdown"}`},
		{"generations", `{"prompt":"cat","model":"gemini-3.6-flash"}`},
		{"generations", `{"prompt":"cat","size":"3840x2160"}`},
		{"generations", `{"prompt":"cat","image":"https://example.invalid/ref.png"}`},
		{"edits", `{"prompt":"blue"}`}, {"edits", `{"prompt":"blue","image":[]}`},
		{"edits", `{"prompt":"blue","image":[["data:image/png;base64,AA=="]]}`},
		{"edits", `{"prompt":"blue","image":{"file_id":"file_1"}}`},
		{"edits", `{"prompt":"blue","image":"data:text/html;base64,PGh0bWw+"}`},
		{"edits", `{"prompt":"blue","mask":"anything"}`},
		{"generations", `{"prompt":"cat","partial_images":2}`},
	} {
		w := httptest.NewRecorder()
		handleImages(w, httptest.NewRequest("POST", "/v1/images/"+tc.path, strings.NewReader(tc.body)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request_error") {
			t.Errorf("%s: %d %s", tc.body, w.Code, w.Body)
		}
	}
	for _, tc := range []struct {
		err    error
		result *StreamResult
		status int
	}{
		{errors.New("download failed"), nil, 502},
		{nil, &StreamResult{}, 502},
		{nil, &StreamResult{Artifacts: []MediaArtifact{{Mime: "video/mp4", Data: []byte("x")}}}, 502},
		{&PromptTooLongError{Bytes: 100, Budget: 10}, nil, 400},
	} {
		stubGeminiCall(t, "", nil, tc.result, tc.err)
		w := httptest.NewRecorder()
		handleImages(w, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"prompt":"cat","stream":true}`)))
		if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("upstream failure became successful SSE: %d %s", w.Code, w.Body)
		}
	}
}

func TestNewAPIChatUsage(t *testing.T) {
	usage := usageOf("hello", "answer", &StreamResult{Reasoning: "thinking through the answer"})
	w := httptest.NewRecorder()
	s := newSSEWriter(w, "chatcmpl-test", 1, "gemini-test")
	s.SendContent("answer")
	s.Finish("stop", usage)
	chunks := newAPIChunks(t, w.Body.String())
	last := chunks[len(chunks)-1]
	if len(last["choices"].([]any)) != 0 {
		t.Fatal("usage must be a separate empty-choices chunk")
	}
	for _, raw := range []any{usage, last["usage"]} {
		data, _ := json.Marshal(raw)
		var parsed struct {
			CompletionTokens        int `json:"completion_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil || parsed.CompletionTokensDetails.ReasoningTokens == 0 || parsed.CompletionTokens != countTokens("answer") {
			t.Fatalf("new-api cannot parse reasoning usage: %s", data)
		}
	}
}

func TestNewAPIImagesAuthentication(t *testing.T) {
	saved := getAPIKey()
	apiKeyRuntime.Store("test-images-key")
	t.Cleanup(func() { apiKeyRuntime.Store(saved) })
	w := httptest.NewRecorder()
	requireAPIKey(handleImages)(w, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"cat"}`)))
	if w.Code != 401 {
		t.Fatalf("unauthenticated image request: %d", w.Code)
	}
}

func TestNewAPIChannelImageTestPreset(t *testing.T) {
	withPoolCookie(t)
	seen := stubGeminiCall(t, "", nil, &StreamResult{Artifacts: []MediaArtifact{testImageArtifact(t, 2, 2)}}, nil)
	w := httptest.NewRecorder()
	handleImages(w, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(
		`{"model":"gemini-image","prompt":"a cat","n":1,"size":"1024x1024"}`)))
	if w.Code != 200 || seen.modelCfg.Image.aspectRatio() != "1:1" {
		t.Fatalf("new-api channel test preset rejected: %d %s", w.Code, w.Body)
	}
}

func newAPIChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return chunks
}
