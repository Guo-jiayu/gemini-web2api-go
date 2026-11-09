package app

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestReferenceImageRedirects(t *testing.T) {
	pngBytes := testImageArtifact(t, 2, 2).Data
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("reference download included credentials")
		}
		switch r.URL.Path {
		case "/image":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pngBytes)
		case "/loop":
			w.Header().Set("Location", "/loop")
			w.WriteHeader(302)
		case "/file":
			w.Header().Set("Location", "file:///private.png")
			w.WriteHeader(302)
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>sign in</html>"))
		default:
			status, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/redirect/"))
			w.Header().Set("Location", "/image")
			w.WriteHeader(status)
		}
	}))
	defer srv.Close()
	for _, proxied := range []bool{false, true} {
		base, proxy := srv.URL, ""
		if proxied {
			base, proxy = "http://fixture.invalid", srv.URL
		}
		for _, status := range []int{301, 302, 303, 307, 308} {
			u, err := fetchImage(fmt.Sprintf("%s/redirect/%d", base, status), proxy, 1)
			if err != nil || string(u.Data) != string(pngBytes) || u.Mime != "image/png" {
				t.Errorf("proxy=%v redirect=%d: mime=%s err=%v", proxied, status, u.Mime, err)
			}
		}
		for path, message := range map[string]string{"/loop": "too many", "/file": "http(s)", "/html": "not an image"} {
			if _, err := fetchImage(base+path, proxy, 1); err == nil || !strings.Contains(err.Error(), message) {
				t.Errorf("proxy=%v path=%s: err=%v", proxied, path, err)
			}
		}
	}
}

func TestReferenceRejectsNonImagePayload(t *testing.T) {
	pngBytes := testImageArtifact(t, 2, 2).Data
	for _, data := range [][]byte{
		[]byte("<html><body>Sign in to view this image</body></html>"),
		[]byte(`{"error":"expired link"}`),
		[]byte("not an image"),
		pngBytes[:20],
	} {
		url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
		if _, err := decodeDataURL(url, 1); err == nil {
			t.Errorf("non-image or truncated input accepted: %q", data)
		}
	}
}

func TestReferenceUsesActualMime(t *testing.T) {
	// A valid image served with an incorrect/generic MIME should still reach
	// upstream with the right kind, filename and MIME, preserving its bytes.
	pngBytes := testImageArtifact(t, 2, 2).Data
	u, err := newMediaUpload(pngBytes, "image/jpeg", 1)
	if err != nil || u.Mime != "image/png" || u.Name != "image1.png" || u.Kind != 1 {
		t.Fatalf("valid PNG mislabeled as JPEG: %+v, %v", u, err)
	}
	if string(u.Data) != string(pngBytes) {
		t.Fatal("reference image bytes changed")
	}
}

func TestReferenceHTMLErrorMentionsImage(t *testing.T) {
	_, err := newMediaUpload([]byte("<!DOCTYPE html><html>Access denied</html>"), "image/png", 2)
	if err == nil || !strings.Contains(err.Error(), "not an image") {
		t.Fatalf("HTML login page must be rejected clearly, got %v", err)
	}
}
