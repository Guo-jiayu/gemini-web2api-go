package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// new-api 8faf1d45 uses x-goog-api-key and Gemini camelCase DTOs for
// /v1beta/models and generateContent/streamGenerateContent?alt=sse.
func TestGeminiNewAPIHistoryImages(t *testing.T) {
	withPoolCookie(t)
	art := testImageArtifact(t, 3, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", art.Mime)
		w.Write(art.Data)
	}))
	defer srv.Close()
	for _, role := range []string{"user", "model"} {
		for _, source := range []string{"inlineData", "fileData"} {
			for _, caption := range []string{"", "previous image"} {
				t.Run(role+"/"+source+"/"+caption, func(t *testing.T) {
					media := map[string]any{"inlineData": map[string]any{"mimeType": art.Mime, "data": base64.StdEncoding.EncodeToString(art.Data)}}
					if source == "fileData" {
						media = map[string]any{"fileData": map[string]any{"mimeType": art.Mime, "fileUri": srv.URL}}
					}
					body, _ := json.Marshal(map[string]any{"contents": []any{
						map[string]any{"role": role, "parts": []any{map[string]any{"text": caption}, media}},
						map[string]any{"role": "user", "parts": []any{map[string]any{"text": "make the background blue"}}},
					}})
					seen := stubGeminiCall(t, "edited", nil, &StreamResult{Artifacts: []MediaArtifact{art}}, nil)
					w := httptest.NewRecorder()
					handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-image:generateContent", bytes.NewReader(body)))
					if w.Code != 200 || len(seen.images) != 1 || !bytes.Equal(seen.images[0].Data, art.Data) {
						t.Fatalf("history image lost: status=%d images=%d body=%s", w.Code, len(seen.images), w.Body)
					}
					if role == "model" && seen.messages[0]["role"] != "assistant" {
						t.Fatal("model history role changed")
					}
				})
			}
		}
	}
}

func TestGeminiNewAPIResponseModalities(t *testing.T) {
	withPoolCookie(t)
	art := testImageArtifact(t, 2, 2)
	caption := "Here is your image."
	inner := make([]interface{}, 45)
	inner[4] = []interface{}{[]interface{}{"rc_test", []string{caption}}}
	payload, _ := json.Marshal(inner)
	raw := testFrame(t, string(payload))
	for _, modalities := range [][]string{nil, {"TEXT", "IMAGE"}, {"IMAGE"}} {
		for _, action := range []string{"generateContent", "streamGenerateContent?alt=sse", "streamGenerateContent"} {
			t.Run(strings.Join(modalities, "+")+"/"+action, func(t *testing.T) {
				saved := runGeminiCall
				runGeminiCall = func(g *geminiCall, delta, reasoning func(string)) (string, []ToolCall, *StreamResult, error) {
					if g.modelCfg.Image.AspectWidth != 1 || g.modelCfg.Image.ResolutionK != 2 {
						t.Fatal("Gemini imageConfig was not forwarded")
					}
					res := &StreamResult{Raw: raw, Artifacts: []MediaArtifact{art}}
					if delta != nil {
						delta(caption)
						res.Emitted = caption
					}
					return caption, nil, res, nil
				}
				t.Cleanup(func() { runGeminiCall = saved })
				body, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "draw"}}}},
					"generationConfig": map[string]any{"responseModalities": modalities, "imageConfig": map[string]any{"aspectRatio": "1:1", "imageSize": "2K"}}})
				w := httptest.NewRecorder()
				handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/gemini-image:"+action, bytes.NewReader(body)))
				if w.Code != 200 {
					t.Fatalf("%d: %s", w.Code, w.Body)
				}
				var chunks []map[string]interface{}
				switch action {
				case "generateContent":
					var obj map[string]interface{}
					if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
						t.Fatal(err)
					}
					chunks = append(chunks, obj)
				case "streamGenerateContent":
					if err := json.Unmarshal(w.Body.Bytes(), &chunks); err != nil {
						t.Fatal(err)
					}
				default:
					chunks = sseChunks(t, w.Body.String())
				}
				images, text := 0, ""
				for _, c := range chunks {
					for _, v := range chunkParts(c) {
						p := v.(map[string]interface{})
						if d, ok := p["inlineData"].(map[string]interface{}); ok {
							images++
							b, err := base64.StdEncoding.DecodeString(d["data"].(string))
							if err != nil || d["mimeType"] != art.Mime || !bytes.Equal(b, art.Data) {
								t.Fatal("invalid inlineData")
							}
						}
						text += getStr(p, "text")
					}
				}
				wantText := caption
				if len(modalities) == 1 {
					wantText = ""
				}
				last := chunks[len(chunks)-1]
				candidate := last["candidates"].([]interface{})[0].(map[string]interface{})
				if images != 1 || text != wantText || candidate["finishReason"] != "STOP" || last["usageMetadata"] == nil {
					t.Fatalf("invalid media response: images=%d text=%q last=%v", images, text, last)
				}
			})
		}
	}
}

func TestGeminiNewAPIRejectsUnsupportedModalities(t *testing.T) {
	for _, c := range []struct{ model, modalities string }{
		{"gemini-4-hydrogen", `["IMAGE"]`},
		{"gemini-image", `["TEXT"]`},
		{"gemini-image", `["AUDIO"]`},
	} {
		w := httptest.NewRecorder()
		body := `{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"responseModalities":` + c.modalities + `}}`
		handleGeminiModelItem(w, httptest.NewRequest("POST", "/v1beta/models/"+c.model+":generateContent", strings.NewReader(body)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"status":"INVALID_ARGUMENT"`) {
			t.Fatalf("%d: %s", w.Code, w.Body)
		}
	}
}

func TestGeminiNewAPIAuthenticationErrors(t *testing.T) {
	saved := getAPIKey()
	apiKeyRuntime.Store("test-gemini-key")
	t.Cleanup(func() { apiKeyRuntime.Store(saved) })
	h := requireAPIKey(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, path := range []string{"/v1beta/models", "/v1beta/models/gemini-image:generateContent"} {
		for _, key := range []string{"", "incorrect", "test-gemini-key"} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("x-goog-api-key", key)
			h(w, r)
			if key == "test-gemini-key" {
				if w.Code != 204 {
					t.Fatal(w.Code)
				}
				continue
			}
			var obj struct {
				Error struct {
					Code   int    `json:"code"`
					Status string `json:"status"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil || obj.Error.Code != 401 || obj.Error.Status != "UNAUTHENTICATED" || w.Code != 401 {
				t.Fatalf("not a Gemini authentication error: %s", w.Body)
			}
		}
	}
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/v1/models", nil))
	if !strings.Contains(w.Body.String(), `"code":"invalid_api_key"`) {
		t.Fatal("OpenAI error contract changed")
	}
}
