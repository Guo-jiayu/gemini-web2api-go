package app

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strconv"
	"strings"
)

// ImageOptions is copied with ModelConfig for each request. Never change the
// runtime/global upscale flag to implement a client's image parameters.
type ImageOptions struct {
	ResolutionK               int
	AspectWidth, AspectHeight int
}

var supportedImageRatios = map[string]bool{
	"1:1": true, "16:9": true, "9:16": true, "4:3": true, "3:4": true,
	"3:2": true, "2:3": true, "4:5": true, "5:4": true, "7:3": true, // 21:9
}

func imageGCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func parseImagePair(value, separator string) (int, int, error) {
	parts := strings.Split(value, separator)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected two positive integers separated by %q", separator)
	}
	w, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if e1 != nil || e2 != nil || w < 1 || h < 1 || w > 8192 || h > 8192 {
		return 0, 0, fmt.Errorf("image dimensions/ratio components must be integers between 1 and 8192")
	}
	if w > h*8 || h > w*8 {
		return 0, 0, fmt.Errorf("image aspect ratio must be between 1:8 and 8:1")
	}
	return w, h, nil
}

// parseImageOptions accepts the common OpenAI extensions and Gemini imageConfig.
// Conflicting aliases are rejected instead of depending on map iteration order.
func parseImageOptions(req map[string]interface{}) (ImageOptions, error) {
	var out ImageOptions
	var visit func(map[string]interface{}, int) error
	setRatio := func(w, h int) error {
		d := imageGCD(w, h)
		w, h = w/d, h/d
		if !supportedImageRatios[fmt.Sprintf("%d:%d", w, h)] {
			return fmt.Errorf("unsupported aspect ratio; use 1:1, 16:9, 9:16, 4:3, 3:4, 3:2, 2:3, 4:5, 5:4 or 21:9")
		}
		if out.AspectWidth != 0 && (out.AspectWidth != w || out.AspectHeight != h) {
			return fmt.Errorf("conflicting image aspect ratios (including size)")
		}
		out.AspectWidth, out.AspectHeight = w, h
		return nil
	}
	visit = func(m map[string]interface{}, depth int) error {
		if depth > 4 {
			return fmt.Errorf("image configuration is nested too deeply")
		}
		for _, key := range []string{"image_size", "imageSize", "resolution", "size", "aspect_ratio", "aspectRatio"} {
			raw, exists := m[key]
			if !exists {
				continue
			}
			value, ok := raw.(string)
			if !ok {
				return fmt.Errorf("%s must be a string", key)
			}
			value = strings.ToUpper(strings.TrimSpace(value))
			if value == "AUTO" {
				continue
			}
			if key == "aspect_ratio" || key == "aspectRatio" || (key == "size" && strings.Contains(value, ":")) {
				w, h, err := parseImagePair(value, ":")
				if err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				if err := setRatio(w, h); err != nil {
					return err
				}
				continue
			}
			if key == "size" && (strings.Contains(value, "X") || strings.Contains(value, "×")) {
				return fmt.Errorf("pixel dimensions are not supported; use aspect_ratio such as 1:1 or 16:9 and image_size such as 1K, 2K or 4K")
			}
			k, err := strconv.Atoi(strings.TrimSuffix(value, "K"))
			if err != nil || !strings.HasSuffix(value, "K") || k < 1 || k > 8 {
				return fmt.Errorf("%s must be 1K through 8K; size also accepts a supported aspect ratio", key)
			}
			if out.ResolutionK != 0 && out.ResolutionK != k {
				return fmt.Errorf("conflicting image resolutions")
			}
			out.ResolutionK = k
		}
		for _, key := range []string{"image_config", "imageConfig", "generation_config", "generationConfig", "extra_body", "google"} {
			if raw, exists := m[key]; exists {
				child, ok := raw.(map[string]interface{})
				if !ok {
					return fmt.Errorf("%s must be an object", key)
				}
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := visit(req, 0)
	return out, err
}

func configureImageModel(mc ModelConfig, req map[string]interface{}) (ModelConfig, error) {
	if mc.Tool != toolImage {
		return mc, nil
	}
	options, err := parseImageOptions(req)
	mc.Image = options
	return mc, err
}

func (o ImageOptions) upscale(globalDefault bool) bool {
	if o.ResolutionK >= 2 {
		return true
	}
	if o.ResolutionK != 0 {
		return false
	}
	return globalDefault
}

func (o ImageOptions) aspectRatio() string {
	if o.AspectWidth == 0 {
		return "auto"
	}
	if o.AspectWidth == 7 && o.AspectHeight == 3 {
		return "21:9"
	}
	return fmt.Sprintf("%d:%d", o.AspectWidth, o.AspectHeight)
}

func (o ImageOptions) promptSuffix() string {
	if o.AspectWidth == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n[Image output requirements]\nGenerate the image with a %s width-to-height aspect ratio. Compose the entire image for this canvas, without letterboxing, borders, or padding. This output aspect ratio takes precedence over any conflicting aspect ratio in the prompt.", o.aspectRatio())
}

// Gemini emits nearby pixel grids (e.g. 1376x768 for 16:9). Allow 2%
// relative ratio error, but never silently return a square for a wide request.
// No resizing/cropping is performed here.
func (o ImageOptions) validateArtifacts(arts []MediaArtifact) error {
	if o.AspectWidth == 0 {
		return nil
	}
	for _, art := range arts {
		c, _, err := image.DecodeConfig(bytes.NewReader(art.Data))
		if err != nil {
			return fmt.Errorf("cannot verify generated image aspect ratio: %w", err)
		}
		want := float64(o.AspectWidth) / float64(o.AspectHeight)
		actual := float64(c.Width) / float64(c.Height)
		if c.Height <= 0 || math.Abs(actual/want-1) > 0.02 {
			return fmt.Errorf("image_aspect_ratio_mismatch: requested %s, upstream returned %dx%d; image was not resized", o.aspectRatio(), c.Width, c.Height)
		}
	}
	return nil
}

func downloadRequestedImages(options ImageOptions, globalDefault bool,
	original, preview func() ([]MediaArtifact, error)) ([]MediaArtifact, error) {
	mode := "preview"
	var arts []MediaArtifact
	var err error
	if options.upscale(globalDefault) {
		mode = "original"
		arts, err = original()
		if err == nil && len(arts) == 0 {
			err = fmt.Errorf("no images in original download")
		}
		if err != nil {
			// An explicit high-resolution request must not silently fall back to
			// the preview. Preserve legacy fallback for the global default only.
			if options.ResolutionK >= 2 {
				return nil, fmt.Errorf("image_original_download_failed: %w", err)
			}
			logf("[upscale] 放大失败，改下原图: %v", err)
			mode = "preview"
		}
	}
	if mode == "preview" {
		arts, err = preview()
	}
	if err != nil {
		return nil, err
	}
	if err := options.validateArtifacts(arts); err != nil {
		return nil, err
	}
	logf("[image-output] download=%s requested_resolution=%dK requested_ratio=%s images=%d", mode, options.ResolutionK, options.aspectRatio(), len(arts))
	return arts, nil
}
