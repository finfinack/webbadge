package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeNFCTagCloner struct {
	events []func(report func(stage string, percent int, message string))
	err    error
}

func (fake *fakeNFCTagCloner) Clone(_ context.Context, report func(stage string, percent int, message string)) error {
	for _, event := range fake.events {
		event(report)
	}
	return fake.err
}

func newNFCTagCloneTestHandler(cloner nfcTagCloner) http.Handler {
	service := &api{
		nfcSlots:    make(chan struct{}, 1),
		nfcTagClone: cloner,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/nfc/tag-clone", service.writeNFCTagClone)
	return securityHeaders(mux)
}

func TestNFCTagCloneEndpointRequiresRequestHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-clone", nil)
	response := httptest.NewRecorder()

	newNFCTagCloneTestHandler(&fakeNFCTagCloner{}).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestNFCTagCloneEndpointStreamsProgress(t *testing.T) {
	fake := &fakeNFCTagCloner{events: []func(report func(stage string, percent int, message string)){
		func(report func(stage string, percent int, message string)) {
			report("waiting-source", 0, "Place the source tag on ACR1552 PICC…")
		},
		func(report func(stage string, percent int, message string)) {
			report("source-read", 40, "Read Type 4 (NDEF filesystem) tag (12 NDEF bytes). Remove it and place the destination tag on ACR1552 PICC…")
		},
		func(report func(stage string, percent int, message string)) {
			report("writing", 60, "Writing to destination tag…")
		},
	}}
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-clone", nil)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTagCloneTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
	got := response.Body.String()
	for _, want := range []string{
		`"stage":"waiting-source"`,
		`"stage":"source-read"`,
		`"stage":"writing"`,
		`data: {"stage":"done","percent":100,"message":"Tag clone completed."}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("response body %q does not contain %q", got, want)
		}
	}
}

func TestNFCTagCloneEndpointStreamsClonerError(t *testing.T) {
	fake := &fakeNFCTagCloner{err: errTestWrite}
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-clone", nil)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTagCloneTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	got := response.Body.String()
	if !strings.Contains(got, `"stage":"error"`) {
		t.Fatalf("response body %q does not contain an error stage", got)
	}
	if !strings.Contains(got, errTestWrite.Error()) {
		t.Fatalf("response body %q does not contain the cloner error", got)
	}
}
