package app

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OpenAI Images uses data[].b64_json, not the Markdown used by Chat.
// Keep generation on the same account, upload and artifact pipeline as Gemini.
const maxImageRequestBytes = 64 << 20

func readImageRequest(w http.ResponseWriter, r *http.Request) (map[string]any, []pendingUpload, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImageRequestBytes)
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil && r.Header.Get("Content-Type") != "" {
		return nil, nil, fmt.Errorf("invalid Content-Type")
	}
	if contentType != "multipart/form-data" {
		var req map[string]any
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil || req == nil {
			return nil, nil, fmt.Errorf("expected a JSON object")
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, nil, fmt.Errorf("expected a single JSON object")
		}
		return req, nil, nil
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		return nil, nil, fmt.Errorf("invalid or oversized multipart body: %w", err)
	}
	defer r.MultipartForm.RemoveAll()
	req := map[string]any{}
	for key, values := range r.MultipartForm.Value {
		if len(values) != 1 {
			return nil, nil, fmt.Errorf("duplicate field %s", key)
		}
		req[key] = values[0]
		if key == "n" || key == "stream" {
			var v any
			if err := json.Unmarshal([]byte(values[0]), &v); err != nil {
				return nil, nil, fmt.Errorf("invalid %s", key)
			}
			req[key] = v
		}
	}
	var images []pendingUpload
	for key := range r.MultipartForm.File {
		if key != "image" && key != "image[]" {
			return nil, nil, fmt.Errorf("unsupported file field %s; use image or image[] (mask is not supported)", key)
		}
	}
	for _, key := range []string{"image", "image[]"} {
		for _, header := range r.MultipartForm.File[key] {
			if header.Size > maxImageBytes {
				return nil, nil, fmt.Errorf("reference image exceeds %d bytes", maxImageBytes)
			}
			f, err := header.Open()
			if err != nil {
				return nil, nil, fmt.Errorf("cannot open reference image: %w", err)
			}
			data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
			f.Close()
			if err != nil {
				return nil, nil, fmt.Errorf("cannot read reference image: %w", err)
			}
			up, err := newMediaUpload(data, http.DetectContentType(data), len(images)+1)
			if err != nil || up.Kind != 1 {
				return nil, nil, fmt.Errorf("invalid reference image: %v", err)
			}
			images = append(images, up)
		}
	}
	return req, images, nil
}

// imageReferenceSources accepts OpenAI JSON edits: image as a URL, an object
// with image_url/url, or an array; images is also used by compatible clients.
func imageReferenceSources(raw any) ([]string, error) {
	if items, ok := raw.([]any); ok {
		var sources []string
		for _, item := range items {
			if _, nested := item.([]any); nested {
				return nil, fmt.Errorf("nested image arrays are not supported")
			}
			part, err := imageReferenceSources(item)
			if err != nil {
				return nil, err
			}
			sources = append(sources, part...)
		}
		return sources, nil
	}
	var source string
	switch value := raw.(type) {
	case string:
		source = value
	case map[string]any:
		source = firstNonEmpty(getStr(value, "image_url"), getStr(value, "url"))
	}
	if strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("image must contain a data: URL or http(s) image_url; file_id is not supported")
	}
	return []string{source}, nil
}

func writeImageError(w http.ResponseWriter, status int, message string) {
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "upstream_error"
	} else if status == 429 {
		kind = "rate_limit_exceeded"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind}})
}

func handleImages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		handleOptions(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeImageError(w, 405, "method not allowed")
		return
	}
	req, images, err := readImageRequest(w, r)
	if err != nil {
		writeImageError(w, 400, err.Error())
		return
	}
	prompt := getStr(req, "prompt")
	if strings.TrimSpace(prompt) == "" {
		writeImageError(w, 400, "prompt is required")
		return
	}
	if raw, exists := req["n"]; exists && raw != float64(1) {
		writeImageError(w, 400, "n must be 1; upstream returns a single candidate")
		return
	}
	stream := false
	if raw, exists := req["stream"]; exists {
		var ok bool
		stream, ok = raw.(bool)
		if !ok {
			writeImageError(w, 400, "stream must be a boolean")
			return
		}
	}
	format := "b64_json"
	if raw, exists := req["response_format"]; exists {
		var ok bool
		format, ok = raw.(string)
		if !ok || (format != "b64_json" && format != "url") {
			writeImageError(w, 400, "response_format must be b64_json or url")
			return
		}
	}
	// Do not silently promise masked edits, encoding or partial-image previews.
	for _, key := range []string{"mask", "output_format", "output_compression", "partial_images", "background", "input_fidelity"} {
		if _, exists := req[key]; exists {
			writeImageError(w, 400, key+" is not supported by the Gemini web image backend")
			return
		}
	}
	model := "gemini-image"
	if raw, exists := req["model"]; exists {
		var ok bool
		model, ok = raw.(string)
		if !ok || model == "" {
			writeImageError(w, 400, "model must be a non-empty string")
			return
		}
	}
	name, mc, err := resolveModel(model)
	if err != nil {
		writeImageError(w, 400, err.Error())
		return
	}
	if mc.Tool != toolImage {
		writeImageError(w, 400, "model must be an image model (gemini-image)")
		return
	}
	// new-api's channel test always sends 1024x1024. Translate the common
	// OpenAI canvas presets to the web backend's ratio control. Pixel-exact
	// resizing is not available; image_size remains the download policy.
	if size, ok := req["size"].(string); ok {
		ratios := map[string]string{
			"256x256": "1:1", "512x512": "1:1", "1024x1024": "1:1",
			"1536x1024": "3:2", "1024x1536": "2:3",
			"1792x1024": "16:9", "1024x1792": "9:16",
		}
		if ratio, exists := ratios[strings.ToLower(strings.TrimSpace(size))]; exists {
			req["size"] = ratio
		}
	}
	mc, err = configureImageModel(mc, req)
	if err != nil {
		writeImageError(w, 400, err.Error())
		return
	}
	var sources []string
	for _, key := range []string{"image", "images"} {
		if raw, exists := req[key]; exists {
			part, err := imageReferenceSources(raw)
			if err != nil {
				writeImageError(w, 400, err.Error())
				return
			}
			sources = append(sources, part...)
		}
	}
	edit := r.URL.Path == "/v1/images/edits"
	if !edit && len(images)+len(sources) > 0 {
		writeImageError(w, 400, "reference images require /v1/images/edits")
		return
	}
	if edit && len(images)+len(sources) == 0 {
		writeImageError(w, 400, "image is required for edits")
		return
	}
	for _, source := range sources {
		up, err := materializeImage(source, "", len(images)+1)
		if err != nil || up.Kind != 1 {
			writeImageError(w, 400, fmt.Sprintf("invalid reference image: %v", err))
			return
		}
		images = append(images, up)
	}
	messages := []map[string]any{{"role": "user", "content": prompt}}
	fullPrompt, latest := messagesToPrompt(messages, nil, nil)
	g := &geminiCall{modelName: name, modelCfg: mc, messages: messages, images: images, prompt: fullPrompt, latest: latest}
	text, _, res, err := runGeminiCall(g, nil, nil)
	if err == nil {
		if res == nil || len(res.Artifacts) == 0 {
			err = errors.New("upstream returned no image artifacts")
		} else {
			for _, art := range res.Artifacts {
				if !strings.HasPrefix(art.Mime, "image/") || len(art.Data) == 0 {
					err = errors.New("upstream returned an empty or non-image artifact")
					break
				}
			}
		}
	}
	endpoint := "images.generations"
	if edit {
		endpoint = "images.edits"
	}
	if err != nil {
		status := callErrorStatus(err)
		recordRequest(endpoint, name, fullPrompt, "", res, status, err.Error(), stream)
		var rate *RateLimitError
		if errors.As(err, &rate) {
			writeRateLimitError(w, rate)
			return
		}
		var downgraded *ModelDowngradedError
		if errors.As(err, &downgraded) && downgraded.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(downgraded.RetryAfter.Seconds()))))
		}
		writeImageError(w, status, err.Error())
		return
	}
	recordRequest(endpoint, name, fullPrompt, text, res, 200, "", stream)
	created := time.Now().Unix()
	data := make([]map[string]any, 0, len(res.Artifacts))
	for _, art := range res.Artifacts {
		encoded := base64.StdEncoding.EncodeToString(art.Data)
		item := map[string]any{"b64_json": encoded}
		if format == "url" {
			// Self-contained URL; never expose Google's authenticated download URL.
			item = map[string]any{"url": "data:" + art.Mime + ";base64," + encoded}
		}
		data = append(data, item)
	}
	if !stream {
		writeJSON(w, 200, map[string]any{"created": created, "data": data})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	event := "image_generation.completed"
	if edit {
		event = "image_edit.completed"
	}
	for _, item := range data {
		item["type"], item["created_at"] = event, created
		b, _ := json.Marshal(item)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
