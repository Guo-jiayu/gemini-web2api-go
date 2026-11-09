package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func imageTestRequest(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestImageParameters(t *testing.T) {
	for _, body := range []string{
		`{"image_size":"2K","aspect_ratio":"16:9"}`,
		`{"resolution":"2k","size":"16:9"}`,
		`{"image_config":{"image_size":"2K","aspect_ratio":"16:9"}}`,
		`{"generationConfig":{"imageConfig":{"imageSize":"2K","aspectRatio":"16:9"}}}`,
		`{"extra_body":{"image_size":"2K","aspect_ratio":"16:9"}}`,
		`{"image_size":"2K","imageSize":"2k","aspect_ratio":"32:18"}`,
	} {
		o, err := parseImageOptions(imageTestRequest(t, body))
		if err != nil || o.ResolutionK != 2 || o.AspectWidth != 16 || o.AspectHeight != 9 {
			t.Errorf("%s -> %+v, %v", body, o, err)
		}
	}
	for _, body := range []string{
		`{"image_size":"4K","resolution":"2K"}`,
		`{"aspect_ratio":"16:9","imageConfig":{"aspectRatio":"1:1"}}`,
		`{"size":"3840x2160"}`, `{"size":"1024×1024"}`,
		`{"aspect_ratio":"0:1"}`, `{"aspect_ratio":"10:1"}`, `{"aspect_ratio":"6:5"}`,
		`{"aspect_ratio":169}`, `{"image_size":2048}`, `{"image_size":"4K please"}`,
		`{"image_size":"0K"}`, `{"image_size":"9K"}`, `{"imageConfig":"4K"}`,
	} {
		if _, err := parseImageOptions(imageTestRequest(t, body)); err == nil {
			t.Errorf("accepted invalid parameters %s", body)
		}
	}
	for _, ratio := range []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "4:5", "5:4", "21:9"} {
		o, err := parseImageOptions(map[string]interface{}{"aspect_ratio": ratio})
		if err != nil || !strings.Contains(o.promptSuffix(), ratio) {
			t.Errorf("ratio %s: %+v %v", ratio, o, err)
		}
	}
}

func TestImageOptionsRequestIsolation(t *testing.T) {
	baseline := Models["gemini-image"]
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resolution := "1K"
			if i%2 == 0 {
				resolution = "4K"
			}
			mc, err := configureImageModel(baseline, map[string]interface{}{"image_size": resolution})
			if err != nil || mc.Image.upscale(false) != (i%2 == 0) || mc.Image.upscale(true) != (i%2 == 0) {
				t.Errorf("request %d lost its options: %+v %v", i, mc.Image, err)
			}
		}(i)
	}
	wg.Wait()
	if Models["gemini-image"] != baseline {
		t.Fatal("shared model definition changed")
	}
	if !(ImageOptions{}).upscale(true) || (ImageOptions{}).upscale(false) {
		t.Fatal("unspecified resolution must honor global default")
	}
	text := Models["gemini-3.6-flash"]
	got, err := configureImageModel(text, map[string]interface{}{"size": "not an image request"})
	if err != nil || got != text {
		t.Fatal("image parameters changed text-model behavior")
	}
}

func testImageArtifact(t *testing.T, w, h int) MediaArtifact {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return MediaArtifact{Mime: "image/png", Data: buf.Bytes()}
}

func TestImageDownloadModeAndRatio(t *testing.T) {
	wide := testImageArtifact(t, 1376, 768)
	square := testImageArtifact(t, 32, 32)
	for _, tc := range []struct {
		name                                             string
		k                                                int
		global, fail, wantOriginal, wantPreview, wantErr bool
	}{
		{"default-preview", 0, false, false, false, true, false},
		{"default-original", 0, true, false, true, false, false},
		{"explicit-1k", 1, true, false, false, true, false},
		{"explicit-2k", 2, false, false, true, false, false},
		{"explicit-4k", 4, false, false, true, false, false},
		{"legacy-fallback", 0, true, true, true, true, false},
		{"no-high-res-fallback", 2, false, true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalCalled, previewCalled := false, false
			got, err := downloadRequestedImages(ImageOptions{ResolutionK: tc.k, AspectWidth: 16, AspectHeight: 9}, tc.global,
				func() ([]MediaArtifact, error) {
					originalCalled = true
					if tc.fail {
						return nil, errors.New("test download failed")
					}
					return []MediaArtifact{wide}, nil
				},
				func() ([]MediaArtifact, error) { previewCalled = true; return []MediaArtifact{wide}, nil })
			if originalCalled != tc.wantOriginal || previewCalled != tc.wantPreview || (err != nil) != tc.wantErr {
				t.Fatalf("calls %v/%v error %v", originalCalled, previewCalled, err)
			}
			if err == nil && !bytes.Equal(got[0].Data, wide.Data) {
				t.Fatal("image bytes changed: no resizing is allowed")
			}
		})
	}
	o := ImageOptions{AspectWidth: 16, AspectHeight: 9}
	if err := o.validateArtifacts([]MediaArtifact{square}); err == nil || !strings.Contains(err.Error(), "image_aspect_ratio_mismatch") {
		t.Fatal("wrong aspect ratio must fail", err)
	}
	if err := o.validateArtifacts([]MediaArtifact{{Data: []byte("not an image")}}); err == nil {
		t.Fatal("invalid image was accepted")
	}
}

func TestImageParametersHTTP(t *testing.T) {
	withPoolCookie(t)
	for _, nativeStream := range []bool{false, true} {
		seen := stubGeminiCall(t, "", nil, &StreamResult{}, errors.New("stop before upstream"))
		body := `{"contents":[{"parts":[{"text":"draw a teapot"}]}],"generationConfig":{"imageConfig":{"imageSize":"4K","aspectRatio":"9:16"}}}`
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if nativeStream {
			handleStreamGenerateContent(w, r, "gemini-image")
		} else {
			handleGenerateContent(w, r, "gemini-image")
		}
		if seen.modelCfg.Image.ResolutionK != 4 || seen.modelCfg.Image.aspectRatio() != "9:16" {
			t.Fatalf("native request lost parameters: %+v", seen.modelCfg.Image)
		}
	}
	for _, endpoint := range []string{"chat", "responses", "native"} {
		w := httptest.NewRecorder()
		body := `{"model":"gemini-image","size":"3840x2160","messages":[{"role":"user","content":"draw"}],"input":"draw"}`
		if endpoint == "native" {
			body = `{"contents":[{"parts":[{"text":"draw"}]}],"generationConfig":{"imageConfig":{"aspectRatio":"6:5"}}}`
		}
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		switch endpoint {
		case "chat":
			handleChatCompletions(w, r)
		case "responses":
			handleResponses(w, r)
		case "native":
			handleGenerateContent(w, r, "gemini-image")
		}
		if w.Code != 400 {
			t.Errorf("%s invalid size status=%d body=%s", endpoint, w.Code, w.Body)
		}
	}
}
