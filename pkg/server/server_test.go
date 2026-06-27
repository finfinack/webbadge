package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webbadge/pkg/badgeimage"
)

func TestPrepareEndpoint(t *testing.T) {
	handler := newTestHandler(t)
	body, contentType := prepareRequestBody(t, "contain", validPNG(t))
	request := httptest.NewRequest(http.MethodPost, "/api/prepare", body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload prepareResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Packed) != badgeimage.PackedSize {
		t.Fatalf("packed size = %d, want %d", len(payload.Packed), badgeimage.PackedSize)
	}
	if len(payload.PreviewPNG) == 0 {
		t.Fatal("preview is empty")
	}
}

func TestPrepareEndpointRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		fit        string
		image      []byte
		wantStatus int
	}{
		{name: "missing request header", fit: "contain", image: validPNG(t), wantStatus: http.StatusForbidden},
		{name: "invalid fit", header: "1", fit: "stretch", image: validPNG(t), wantStatus: http.StatusBadRequest},
		{name: "invalid image", header: "1", fit: "contain", image: []byte("bad"), wantStatus: http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			body, contentType := prepareRequestBody(t, test.fit, test.image)
			request := httptest.NewRequest(http.MethodPost, "/api/prepare", body)
			request.Header.Set("Content-Type", contentType)
			if test.header != "" {
				request.Header.Set("X-WebBadge-Request", test.header)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	headers := []string{
		"Content-Security-Policy",
		"Permissions-Policy",
		"Referrer-Policy",
		"Strict-Transport-Security",
		"X-Content-Type-Options",
		"X-Frame-Options",
	}
	for _, header := range headers {
		if response.Header().Get(header) == "" {
			t.Errorf("%s header is missing", header)
		}
	}
	if strings.Contains(response.Header().Get("Content-Security-Policy"), "unsafe-inline") {
		t.Error("Content-Security-Policy unexpectedly permits inline code")
	}
	if !strings.Contains(response.Header().Get("Permissions-Policy"), "camera=(self)") {
		t.Error("Permissions-Policy does not permit same-origin camera access")
	}
}

func TestBulmaStylesheetIsServedLocally(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/bulma.min.css", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/css") {
		t.Errorf("Content-Type = %q, want CSS", response.Header().Get("Content-Type"))
	}
	if !strings.Contains(response.Body.String(), "bulma.io") {
		t.Error("response does not contain the vendored Bulma stylesheet")
	}
}

func TestDesignControlsAreServed(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	for _, expected := range []string{
		`id="portrait-size"`,
		`value="standard"`,
		`value="full"`,
		`id="monogram-letter"`,
		`id="monogram-color"`,
		`id="open-webcam"`,
		`id="webcam-crop-canvas"`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("response does not contain %q", expected)
		}
	}
}

func TestBrowserLogEndpoint(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/log", strings.NewReader(`{
		"level":"error",
		"event":"ack.timeout",
		"session":"85cc7d0d-0717-4b0d-a598-d1c0bb7ec26a",
		"sequence":17,
		"stage":"initial image header",
		"device":"P1_397_AABBCCDDEEFF",
		"detail":"type=command",
		"block":0,
		"bytes":8
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	for _, expected := range []string{
		`"level":"DEBUG"`,
		`"ble_level":"error"`,
		`"event":"ack.timeout"`,
		`"stage":"initial image header"`,
		`"bytes":8`,
	} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("log %q does not contain %q", logs.String(), expected)
		}
	}
}

func TestBrowserLogEndpointIsHiddenAtInfoLevel(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/log", strings.NewReader(
		`{"level":"error","event":"transfer.failed","session":"test-session","sequence":1}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("BLE log unexpectedly emitted at info level: %s", logs.String())
	}
}

func TestBrowserLogEndpointRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		body       string
		wantStatus int
	}{
		{
			name:       "missing request header",
			body:       `{"level":"info","event":"page.ready","session":"test-session"}`,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "unknown field",
			header:     "1",
			body:       `{"level":"info","event":"page.ready","session":"test-session","secret":"value"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "invalid event",
			header:     "1",
			body:       "{\"level\":\"info\",\"event\":\"line\\nbreak\",\"session\":\"test-session\"}",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "control character in detail",
			header:     "1",
			body:       `{"level":"info","event":"page.ready","session":"test-session","detail":"first\nsecond"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "trailing JSON",
			header:     "1",
			body:       `{"level":"info","event":"page.ready","session":"test-session"} {}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			request := httptest.NewRequest(http.MethodPost, "/api/log", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				request.Header.Set("X-WebBadge-Request", test.header)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return handler
}

func validPNG(t *testing.T) []byte {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for index := 0; index < len(source.Pix); index += 4 {
		source.Pix[index] = 255
		source.Pix[index+1] = 255
		source.Pix[index+2] = 255
		source.Pix[index+3] = 255
	}
	var output bytes.Buffer
	if err := png.Encode(&output, source); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return output.Bytes()
}

func prepareRequestBody(t *testing.T, fit string, imageData []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("fit", fit); err != nil {
		t.Fatalf("write fit field: %v", err)
	}
	file, err := writer.CreateFormFile("image", "badge.png")
	if err != nil {
		t.Fatalf("create image field: %v", err)
	}
	if _, err := file.Write(imageData); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &body, writer.FormDataContentType()
}
