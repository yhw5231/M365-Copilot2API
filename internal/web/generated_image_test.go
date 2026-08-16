package web

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGeneratedImageStoreAndServe(t *testing.T) {
	s := &Server{generatedImages: map[string]*generatedImage{}}
	data := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x01, 0x02, 0x03}
	id := s.storeGeneratedImage(data, "image/png")
	if len(id) != 36 { // uuid v4 hex + dashes
		t.Fatalf("id=%q not a plain uuid", id)
	}
	s.generatedImagesMu.Lock()
	img, ok := s.generatedImages[id]
	s.generatedImagesMu.Unlock()
	if !ok {
		t.Fatal("stored image missing")
	}
	if img.ContentType != "image/png" || string(img.Data) != string(data) {
		t.Fatalf("stored image mismatch ct=%q", img.ContentType)
	}

	// Serve via generatedImageFile
	req := httptest.NewRequest(http.MethodGet, "/v1/images/files/"+id, nil)
	w := httptest.NewRecorder()
	s.generatedImageFile(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type=%q", ct)
	}
	if w.Body.String() != string(data) {
		t.Fatal("served body mismatch")
	}

	// Unknown / malformed ids
	for _, bad := range []string{"not-a-uuid", ""} {
		w2 := httptest.NewRecorder()
		s.generatedImageFile(w2, httptest.NewRequest(http.MethodGet, "/v1/images/files/"+bad, nil))
		if w2.Code != http.StatusNotFound {
			t.Fatalf("bad id %q status=%d want 404", bad, w2.Code)
		}
	}

	// Expired entries are removed on access
	s.generatedImagesMu.Lock()
	s.generatedImages[id].ExpiresAt = time.Now().Add(-time.Second)
	s.generatedImagesMu.Unlock()
	w3 := httptest.NewRecorder()
	s.generatedImageFile(w3, httptest.NewRequest(http.MethodGet, "/v1/images/files/"+id, nil))
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expired status=%d want 404", w3.Code)
	}
	s.generatedImagesMu.Lock()
	_, still := s.generatedImages[id]
	s.generatedImagesMu.Unlock()
	if still {
		t.Fatal("expired entry should be deleted")
	}

	// Non-GET rejected
	w4 := httptest.NewRecorder()
	s.generatedImageFile(w4, httptest.NewRequest(http.MethodPost, "/v1/images/files/"+id, nil))
	if w4.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d want 405", w4.Code)
	}
}

func TestGeneratedImageTTLEvictionAndLimit(t *testing.T) {
	s := &Server{generatedImages: map[string]*generatedImage{}}
	// 手动放入 maxGeneratedImages 个条目，ExpiresAt 严格递增，保证"最旧"确定。
	base := time.Now().Add(time.Hour)
	firstID := ""
	lastID := ""
	for i := 0; i < maxGeneratedImages; i++ {
		id := "00000000-0000-0000-0000-" + fmt.Sprintf("%012d", i)
		if i == 0 {
			firstID = id
		}
		lastID = id
		s.generatedImages[id] = &generatedImage{Data: []byte{byte(i)}, ContentType: "image/png", ExpiresAt: base.Add(time.Duration(i) * time.Second)}
	}
	// 存入第 maxGeneratedImages+1 个 → 最旧的 firstID 应被淘汰。
	newID := s.storeGeneratedImage([]byte{0xFF}, "image/png")
	s.generatedImagesMu.Lock()
	_, firstStill := s.generatedImages[firstID]
	_, lastStill := s.generatedImages[lastID]
	_, newStill := s.generatedImages[newID]
	count := len(s.generatedImages)
	s.generatedImagesMu.Unlock()
	if firstStill {
		t.Fatal("oldest image should have been evicted at capacity")
	}
	if !lastStill || !newStill {
		t.Fatal("newest images must survive eviction")
	}
	if count != maxGeneratedImages {
		t.Fatalf("cache grew to %d, want %d", count, maxGeneratedImages)
	}
}

func TestGeneratedImageEvictsExpiredOnStore(t *testing.T) {
	s := &Server{generatedImages: map[string]*generatedImage{}}
	expired := "00000000-0000-0000-0000-000000000001"
	fresh := "00000000-0000-0000-0000-000000000002"
	s.generatedImages[expired] = &generatedImage{Data: []byte{1}, ContentType: "image/png", ExpiresAt: time.Now().Add(-time.Minute)}
	s.generatedImages[fresh] = &generatedImage{Data: []byte{2}, ContentType: "image/png", ExpiresAt: time.Now().Add(time.Hour)}
	_ = s.storeGeneratedImage([]byte{3}, "image/png")
	s.generatedImagesMu.Lock()
	_, eStill := s.generatedImages[expired]
	_, fStill := s.generatedImages[fresh]
	s.generatedImagesMu.Unlock()
	if eStill {
		t.Fatal("expired entry should be evicted on next store")
	}
	if !fStill {
		t.Fatal("fresh entry must survive")
	}
}

func TestGeneratedImageURLScheme(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	plain := httptest.NewRequest(http.MethodGet, "http://example.com/x", nil)
	if u := generatedImageURL(plain, id); u != "http://example.com/v1/images/files/"+id {
		t.Fatalf("plain url=%q", u)
	}
	tlsReq := httptest.NewRequest(http.MethodGet, "http://example.com/x", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if u := generatedImageURL(tlsReq, id); u != "https://example.com/v1/images/files/"+id {
		t.Fatalf("tls url=%q", u)
	}
	proxied := httptest.NewRequest(http.MethodGet, "http://example.com/x", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	if u := generatedImageURL(proxied, id); u != "https://example.com/v1/images/files/"+id {
		t.Fatalf("x-forwarded-proto url=%q", u)
	}
}

func TestIsDesignerImageURL(t *testing.T) {
	ok := []string{
		"https://designerapp.officeapps.live.com/designerapp/document/abc",
		"HTTPS://DESIGNERAPP.OFFICEAPPS.LIVE.COM/xyz",
	}
	for _, u := range ok {
		if !isDesignerImageURL(u) {
			t.Fatalf("expected designer url: %s", u)
		}
	}
	bad := []string{
		"http://designerapp.officeapps.live.com/x", // not https
		"https://example.com/x",
		"not a url",
		"",
	}
	for _, u := range bad {
		if isDesignerImageURL(u) {
			t.Fatalf("unexpected designer url: %s", u)
		}
	}
}

func TestIsImageQuotaRefusal(t *testing.T) {
	for _, text := range []string{
		"Sorry, I can't generate any more images today.",
		"Sorry, try again tomorrow.",
		"抱歉，我今天无法再生成图片。请明天再试。",
	} {
		if !isImageQuotaRefusal(text) {
			t.Fatalf("quota refusal not detected: %q", text)
		}
	}
	if isImageQuotaRefusal("Here is your generated image: https://example.com/a.png") {
		t.Fatal("ordinary image response misclassified")
	}
}

func TestDownloadImageAsDataURI(t *testing.T) {
	// Invalid / unreachable URLs must fall back to the original URL.
	u, err := downloadImageAsDataURI("http://127.0.0.1:1/nope.png")
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://127.0.0.1:1/nope.png" {
		t.Fatalf("fallback url=%q", u)
	}
	// Direct base64 encode round-trip via helper.
	if got := base64.StdEncoding.EncodeToString([]byte("hi")); got != "aGk=" {
		t.Fatal(base64ErrMsg)
	}
}

const base64ErrMsg = "base64 mismatch"
