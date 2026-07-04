package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"webbadge/pkg/badgeimage"
)

type fakeNFCWriter struct {
	events  []func(report func(stage string, percent int, message string))
	err     error
	ctx     context.Context
	profile badgeimage.Profile
}

func (fake *fakeNFCWriter) Write(ctx context.Context, _ image.Image, profile badgeimage.Profile, report func(stage string, percent int, message string)) error {
	fake.ctx = ctx
	fake.profile = profile
	for _, event := range fake.events {
		event(report)
	}
	return fake.err
}

func newNFCTestHandler(writer nfcBadgeWriter) http.Handler {
	service := &api{
		renderSlots: make(chan struct{}, 2),
		nfcSlots:    make(chan struct{}, 1),
		nfcWriter:   writer,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/nfc/write", service.writeNFC)
	return securityHeaders(mux)
}

type fakeNFCProbe struct {
	status  nfcReaderStatus
	err     error
	profile badgeimage.Profile
}

func (fake *fakeNFCProbe) Probe(_ context.Context, profile badgeimage.Profile) (nfcReaderStatus, error) {
	fake.profile = profile
	return fake.status, fake.err
}

func newNFCStatusTestHandler(probe nfcReaderProbe) http.Handler {
	service := &api{nfcProbe: probe}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/nfc/status", service.nfcStatus)
	return securityHeaders(mux)
}

type fakeNFCTagInfoReader struct {
	info nfcTagInfo
	err  error
}

func (fake *fakeNFCTagInfoReader) ReadInfo(context.Context) (nfcTagInfo, error) {
	return fake.info, fake.err
}

func (fake *fakeNFCTagInfoReader) WriteNDEF(_ context.Context, recordType, value string) error {
	if fake.err != nil {
		return fake.err
	}
	fake.info.Content = recordType + ":" + value
	return nil
}

func newNFCTagInfoTestHandler(reader nfcTagInfoReader) http.Handler {
	service := &api{
		nfcSlots:   make(chan struct{}, 1),
		nfcTagInfo: reader,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/nfc/tag-info", service.readNFCTagInfo)
	return securityHeaders(mux)
}

func newNFCTagNDEFWriteTestHandler(writer nfcTagNDEFWriter) http.Handler {
	service := &api{
		nfcSlots:    make(chan struct{}, 1),
		nfcTagWrite: writer,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/nfc/tag-ndef", service.writeNFCTagNDEF)
	return securityHeaders(mux)
}

func TestNFCStatusEndpoint(t *testing.T) {
	tests := []struct {
		name             string
		probe            fakeNFCProbe
		wantAvailable    bool
		wantBadgePresent bool
	}{
		{
			name:             "reader and badge present",
			probe:            fakeNFCProbe{status: nfcReaderStatus{Reader: "ACR1552 PICC", BadgePresent: true}},
			wantAvailable:    true,
			wantBadgePresent: true,
		},
		{
			name:          "reader present, no badge",
			probe:         fakeNFCProbe{status: nfcReaderStatus{Reader: "ACR1552 PICC"}},
			wantAvailable: true,
		},
		{
			name:  "no reader",
			probe: fakeNFCProbe{err: errTestWrite},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/nfc/status", nil)
			request.Header.Set("X-WebBadge-Request", "1")
			response := httptest.NewRecorder()

			probe := test.probe
			newNFCStatusTestHandler(&probe).ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
			}
			var payload nfcStatusResponse
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload.Available != test.wantAvailable {
				t.Errorf("Available = %v, want %v", payload.Available, test.wantAvailable)
			}
			if payload.BadgePresent != test.wantBadgePresent {
				t.Errorf("BadgePresent = %v, want %v", payload.BadgePresent, test.wantBadgePresent)
			}
			if test.wantAvailable && payload.Reader == "" {
				t.Error("Reader is empty, want a reader name")
			}
			if !test.wantAvailable && payload.Error == "" {
				t.Error("Error is empty, want an error message")
			}
		})
	}
}

func TestNFCStatusEndpointRejectsMissingHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/nfc/status", nil)
	response := httptest.NewRecorder()

	newNFCStatusTestHandler(&fakeNFCProbe{}).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestNFCStatusEndpointPassesSelectedProfile(t *testing.T) {
	probe := &fakeNFCProbe{status: nfcReaderStatus{Reader: "ACR1552 PICC", BadgePresent: true}}
	request := httptest.NewRequest(http.MethodGet, "/api/nfc/status?profile="+badgeimage.NfcInk240x416.ID, nil)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCStatusTestHandler(probe).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if probe.profile != badgeimage.NfcInk240x416 {
		t.Fatalf("profile = %v, want %v", probe.profile, badgeimage.NfcInk240x416)
	}
}

func TestNFCStatusEndpointRejectsUnknownProfile(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/nfc/status?profile=unknown", nil)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCStatusTestHandler(&fakeNFCProbe{}).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

func TestWriteNFCEndpointStreamsProgress(t *testing.T) {
	fake := &fakeNFCWriter{events: []func(report func(stage string, percent int, message string)){
		func(report func(stage string, percent int, message string)) {
			report("waiting", 0, "Waiting for badge…")
		},
		func(report func(stage string, percent int, message string)) {
			report("writing", 50, "Writing to badge… 50%")
		},
	}}
	body, contentType := nfcWriteRequestBody(t, validBadgePNG(t))
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/write", body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
	got := response.Body.String()
	for _, want := range []string{
		`data: {"stage":"waiting","percent":0,"message":"Waiting for badge…"}`,
		`data: {"stage":"writing","percent":50,"message":"Writing to badge… 50%"}`,
		`data: {"stage":"done","percent":100,"message":"Badge write completed."}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("response body %q does not contain %q", got, want)
		}
	}
	if fake.ctx == nil {
		t.Fatal("writer was not invoked with a context")
	}
}

func TestWriteNFCEndpointStreamsWriterError(t *testing.T) {
	fake := &fakeNFCWriter{err: errTestWrite}
	body, contentType := nfcWriteRequestBody(t, validBadgePNG(t))
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/write", body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"stage":"error"`) {
		t.Fatalf("response body %q does not contain an error stage", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), errTestWrite.Error()) {
		t.Fatalf("response body %q does not contain the writer error", response.Body.String())
	}
}

func TestWriteNFCEndpointMapsHybridWrongAreaError(t *testing.T) {
	fake := &fakeNFCWriter{err: &testWriteError{"write badge: wake badge: write FIFO: ACR1552 transparent exchange failed: raw-transmit[card returned status 6700]"}}
	body, contentType := nfcWriteRequestBody(t, validBadgePNG(t))
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/write", body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	want := "regular NFC tag area instead of the e-ink image-writing interface"
	if !strings.Contains(response.Body.String(), want) {
		t.Fatalf("response body %q does not contain %q", response.Body.String(), want)
	}
}

func TestWriteNFCEndpointMapsNfcInkWrongAreaError(t *testing.T) {
	fake := &fakeNFCWriter{err: &testWriteError{"read nfcink config: card returned status 6985"}}

	source := image.NewNRGBA(image.Rect(0, 0, badgeimage.NfcInk240x416.Width, badgeimage.NfcInk240x416.Height))
	for index := 0; index < len(source.Pix); index += 4 {
		source.Pix[index+3] = 255
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, source); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("profile", badgeimage.NfcInk240x416.ID); err != nil {
		t.Fatalf("write profile field: %v", err)
	}
	file, err := writer.CreateFormFile("image", "badge.png")
	if err != nil {
		t.Fatalf("create image field: %v", err)
	}
	if _, err := file.Write(pngBytes.Bytes()); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/nfc/write", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	want := "did not answer like the nfcink image-writing interface"
	if !strings.Contains(response.Body.String(), want) {
		t.Fatalf("response body %q does not contain %q", response.Body.String(), want)
	}
}

func TestWriteNFCEndpointRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		image      []byte
		wantStatus int
	}{
		{name: "missing request header", image: validBadgePNG(t), wantStatus: http.StatusForbidden},
		{name: "invalid image", header: "1", image: []byte("bad"), wantStatus: http.StatusBadRequest},
		{name: "wrong dimensions", header: "1", image: validPNG(t), wantStatus: http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeNFCWriter{}
			body, contentType := nfcWriteRequestBody(t, test.image)
			request := httptest.NewRequest(http.MethodPost, "/api/nfc/write", body)
			request.Header.Set("Content-Type", contentType)
			if test.header != "" {
				request.Header.Set("X-WebBadge-Request", test.header)
			}
			response := httptest.NewRecorder()

			newNFCTestHandler(fake).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
			if fake.ctx != nil {
				t.Fatal("writer was invoked for a request that should have been rejected before streaming")
			}
		})
	}
}

func TestWriteNFCEndpointDispatchesSelectedProfile(t *testing.T) {
	fake := &fakeNFCWriter{}
	source := image.NewNRGBA(image.Rect(0, 0, badgeimage.NfcInk240x416.Width, badgeimage.NfcInk240x416.Height))
	for index := 0; index < len(source.Pix); index += 4 {
		source.Pix[index+3] = 255
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, source); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("profile", badgeimage.NfcInk240x416.ID); err != nil {
		t.Fatalf("write profile field: %v", err)
	}
	file, err := writer.CreateFormFile("image", "badge.png")
	if err != nil {
		t.Fatalf("create image field: %v", err)
	}
	if _, err := file.Write(pngBytes.Bytes()); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/nfc/write", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if fake.profile != badgeimage.NfcInk240x416 {
		t.Fatalf("profile = %v, want %v", fake.profile, badgeimage.NfcInk240x416)
	}
}

func TestReadNFCTagInfoEndpoint(t *testing.T) {
	reader := &fakeNFCTagInfoReader{info: nfcTagInfo{
		Reader:          "ACR1552 PICC",
		ATR:             "3B8F8001804F0CA000000306030001000000006A",
		UID:             "04A1B2C3D4E5",
		ATS:             "067577810280",
		NDEFSupported:   true,
		NDEFWritable:    true,
		NDEFCapacity:    500,
		NDEFRecordType:  "text",
		NDEFRecordValue: "hello",
		Content:         "Text: hello",
		ContentHex:      "D101085402656E68656C6C6F",
		ContentLines:    []string{"Text: hello"},
		ProbeLines: []string{
			"Type 4 NDEF application select: status 6A81",
			"Storage-card read block/page 0: ok data=04010203",
		},
	}}
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-info", nil)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTagInfoTestHandler(reader).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload nfcTagInfo
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Reader != reader.info.Reader || payload.UID != reader.info.UID || payload.ATS != reader.info.ATS {
		t.Fatalf("payload = %+v, want %+v", payload, reader.info)
	}
	if payload.Content != reader.info.Content || payload.ContentHex != reader.info.ContentHex {
		t.Fatalf("payload content = %+v, want %+v", payload, reader.info)
	}
	if !payload.NDEFSupported || !payload.NDEFWritable || payload.NDEFCapacity != 500 {
		t.Fatalf("payload ndef = %+v, want supported+writable capacity 500", payload)
	}
	if payload.NDEFRecordType != "text" || payload.NDEFRecordValue != "hello" {
		t.Fatalf("payload editable ndef = %+v, want text/hello", payload)
	}
	if len(payload.ProbeLines) != len(reader.info.ProbeLines) {
		t.Fatalf("probe lines = %#v, want %#v", payload.ProbeLines, reader.info.ProbeLines)
	}
}

func TestReadNFCTagInfoEndpointRejectsMissingHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-info", nil)
	response := httptest.NewRecorder()

	newNFCTagInfoTestHandler(&fakeNFCTagInfoReader{}).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestReadNFCTagInfoEndpointSurfacesReaderError(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-info", nil)
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTagInfoTestHandler(&fakeNFCTagInfoReader{err: errTestWrite}).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), errTestWrite.Error()) {
		t.Fatalf("response body %q does not contain %q", response.Body.String(), errTestWrite.Error())
	}
}

func TestWriteNFCTagNDEFEndpoint(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-ndef", strings.NewReader(`{"type":"url","value":"https://example.com"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTagNDEFWriteTestHandler(&fakeNFCTagInfoReader{}).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
}

func TestWriteNFCTagNDEFEndpointRejectsMissingHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-ndef", strings.NewReader(`{"type":"text","value":"hi"}`))
	response := httptest.NewRecorder()

	newNFCTagNDEFWriteTestHandler(&fakeNFCTagInfoReader{}).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestWriteNFCTagNDEFEndpointSurfacesWriterError(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nfc/tag-ndef", strings.NewReader(`{"type":"text","value":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WebBadge-Request", "1")
	response := httptest.NewRecorder()

	newNFCTagNDEFWriteTestHandler(&fakeNFCTagInfoReader{err: errTestWrite}).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

func TestParseCapabilityContainer(t *testing.T) {
	data := []byte{
		0x00, 0x0F,
		0x20,
		0x00, 0x3B,
		0x00, 0x34,
		0x04, 0x06,
		0xE1, 0x04,
		0x01, 0xF4,
		0x00, 0xFF,
	}
	capability, err := parseCapabilityContainer(data)
	if err != nil {
		t.Fatalf("parseCapabilityContainer() error = %v", err)
	}
	if got := fmt.Sprintf("%X", capability.FileID); got != "E104" {
		t.Fatalf("fileID = %s, want E104", got)
	}
	if capability.MaxSize != 500 {
		t.Fatalf("maxSize = %d, want 500", capability.MaxSize)
	}
	if capability.Writable {
		t.Fatal("Writable = true, want false for write access FF")
	}
}

func TestSummarizeNDEFTextRecord(t *testing.T) {
	data := []byte{0xD1, 0x01, 0x08, 0x54, 0x02, 0x65, 0x6E, 'h', 'e', 'l', 'l', 'o'}
	summary, lines := summarizeNDEF(data)
	if summary != "Text: hello" {
		t.Fatalf("summary = %q, want %q", summary, "Text: hello")
	}
	if len(lines) != 1 || lines[0] != "Text: hello" {
		t.Fatalf("lines = %#v, want [\"Text: hello\"]", lines)
	}
}

func TestEncodeNDEFMessage(t *testing.T) {
	message, err := encodeNDEFMessage("email", "person@example.com")
	if err != nil {
		t.Fatalf("encodeNDEFMessage() error = %v", err)
	}
	if got := fmt.Sprintf("%X", message); !strings.Contains(got, "55") {
		t.Fatalf("message = %s, want URI record", got)
	}
}

func TestExtractEditableNDEFRecord(t *testing.T) {
	recordType, value := extractEditableNDEFRecord([]byte{0xD1, 0x01, 0x08, 0x54, 0x02, 0x65, 0x6E, 'h', 'e', 'l', 'l', 'o'})
	if recordType != "text" || value != "hello" {
		t.Fatalf("extractEditableNDEFRecord() = %q, %q; want %q, %q", recordType, value, "text", "hello")
	}
}

func TestParseType2TLV(t *testing.T) {
	payload, err := parseType2TLV([]byte{
		0x00, 0x00,
		0x03, 0x19,
		0xD1, 0x01, 0x15, 0x55, 0x04, 'y', 'o', 'u', 't', 'u', '.', 'b', 'e', '/', 'd', 'Q', 'w', '4', 'w', '9', 'W', 'g', 'X', 'c', 'Q',
		0xFE,
	})
	if err != nil {
		t.Fatalf("parseType2TLV() error = %v", err)
	}
	if got := fmt.Sprintf("%X", payload[:4]); got != "D1011555" {
		t.Fatalf("payload prefix = %s, want D1011555", got)
	}
}

var errTestWrite = &testWriteError{"badge refused the write"}

type testWriteError struct{ message string }

func (err *testWriteError) Error() string { return err.message }

func validBadgePNG(t *testing.T) []byte {
	t.Helper()
	source := image.NewNRGBA(image.Rect(0, 0, badgeimage.Width, badgeimage.Height))
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

func nfcWriteRequestBody(t *testing.T, imageData []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("profile", badgeimage.Hybrid397.ID); err != nil {
		t.Fatalf("write profile field: %v", err)
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
