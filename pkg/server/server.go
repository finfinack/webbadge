// Package server provides the HTTP API and embedded browser application.
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"webbadge/pkg/badgeimage"
)

const (
	maxUploadBytes     = 10 << 20
	maxRequestBytes    = maxUploadBytes + (1 << 20)
	maxBrowserLogBytes = 4 << 10
)

var logIdentifierPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`)

//go:embed web/*
var webFiles embed.FS

type api struct {
	renderSlots chan struct{}
	nfcSlots    chan struct{}
	nfcWriter   nfcBadgeWriter
	nfcProbe    nfcReaderProbe
}

type prepareResponse struct {
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	PreviewPNG []byte `json:"previewPng"`
	Packed     []byte `json:"packed"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type browserLog struct {
	Level    string `json:"level"`
	Event    string `json:"event"`
	Session  string `json:"session"`
	Sequence int    `json:"sequence"`
	Stage    string `json:"stage"`
	Device   string `json:"device"`
	Detail   string `json:"detail"`
	Block    int    `json:"block"`
	Bytes    int    `json:"bytes"`
}

// New constructs the complete HTTP handler.
func New() (http.Handler, error) {
	staticFiles, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, fmt.Errorf("open embedded frontend: %w", err)
	}

	service := &api{
		renderSlots: make(chan struct{}, 2),
		nfcSlots:    make(chan struct{}, 1),
		nfcWriter:   pcscNFCWriter{},
		nfcProbe:    pcscReaderProbe{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/prepare", service.prepare)
	mux.HandleFunc("POST /api/log", service.browserLog)
	mux.HandleFunc("POST /api/nfc/write", service.writeNFC)
	mux.HandleFunc("GET /api/nfc/status", service.nfcStatus)
	mux.Handle("GET /", http.FileServer(http.FS(staticFiles)))
	return securityHeaders(mux), nil
}

func (service *api) browserLog(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxBrowserLogBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var entry browserLog
	if err := decoder.Decode(&entry); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid browser log")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid browser log")
		return
	}
	if err := validateBrowserLog(entry); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid browser log")
		return
	}

	attributes := []slog.Attr{
		slog.String("component", "web_bluetooth"),
		slog.String("ble_level", entry.Level),
		slog.String("event", entry.Event),
		slog.String("session", entry.Session),
		slog.Int("sequence", entry.Sequence),
	}
	if entry.Stage != "" {
		attributes = append(attributes, slog.String("stage", entry.Stage))
	}
	if entry.Device != "" {
		attributes = append(attributes, slog.String("device", entry.Device))
	}
	if entry.Detail != "" {
		attributes = append(attributes, slog.String("detail", entry.Detail))
	}
	if entry.Block > 0 {
		attributes = append(attributes, slog.Int("block", entry.Block))
	}
	if entry.Bytes > 0 {
		attributes = append(attributes, slog.Int("bytes", entry.Bytes))
	}

	// Browser BLE telemetry is intentionally debug-only. The original browser
	// severity is retained as an attribute for diagnostics when debug logging
	// is explicitly enabled.
	slog.LogAttrs(request.Context(), slog.LevelDebug, "browser BLE", attributes...)
	writer.WriteHeader(http.StatusNoContent)
}

func validateBrowserLog(entry browserLog) error {
	if entry.Level != "debug" && entry.Level != "info" &&
		entry.Level != "warn" && entry.Level != "error" {
		return errors.New("unsupported log level")
	}
	if !validLogIdentifier(entry.Event, 64) ||
		!validLogIdentifier(entry.Session, 64) {
		return errors.New("invalid log identifier")
	}
	if entry.Sequence < 0 || entry.Sequence > 1_000_000 ||
		entry.Block < 0 || entry.Block > 255 ||
		entry.Bytes < 0 || entry.Bytes > 16<<20 {
		return errors.New("numeric log field is out of range")
	}
	if !validLogText(entry.Stage, 96) ||
		!validLogText(entry.Device, 160) ||
		!validLogText(entry.Detail, 768) {
		return errors.New("log text is invalid")
	}
	return nil
}

func validLogIdentifier(value string, maximum int) bool {
	return value != "" && len(value) <= maximum &&
		utf8.ValidString(value) && logIdentifierPattern.MatchString(value)
}

func validLogText(value string, maximum int) bool {
	if len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (service *api) prepare(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(writer, http.StatusRequestEntityTooLarge, "image upload is too large")
			return
		}
		writeError(writer, http.StatusBadRequest, "invalid multipart request")
		return
	}
	if request.MultipartForm != nil {
		defer func() {
			if err := request.MultipartForm.RemoveAll(); err != nil {
				slog.Warn("remove multipart temporary files", "error", err)
			}
		}()
	}

	fit := badgeimage.Fit(request.FormValue("fit"))
	if fit != badgeimage.FitContain && fit != badgeimage.FitCover {
		writeError(writer, http.StatusBadRequest, "fit must be contain or cover")
		return
	}

	profile := badgeimage.Hybrid397
	if profileID := request.FormValue("profile"); profileID != "" {
		var ok bool
		profile, ok = badgeimage.ProfileByID(profileID)
		if !ok {
			writeError(writer, http.StatusBadRequest, "unknown badge profile")
			return
		}
	}

	file, _, err := request.FormFile("image")
	if err != nil {
		writeError(writer, http.StatusBadRequest, "one image file is required")
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Warn("close uploaded image", "error", closeErr)
		}
	}()

	imageData, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "could not read uploaded image")
		return
	}
	if len(imageData) > maxUploadBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "image upload is too large")
		return
	}

	select {
	case service.renderSlots <- struct{}{}:
		defer func() { <-service.renderSlots }()
	case <-request.Context().Done():
		return
	}

	result, err := badgeimage.Prepare(imageData, fit, profile)
	if err != nil {
		slog.Info("rejected image", "error", err)
		writeError(writer, http.StatusBadRequest, "image must be a valid PNG or JPEG within the size limits")
		return
	}

	writeJSON(writer, http.StatusOK, prepareResponse{
		Width:      profile.Width,
		Height:     profile.Height,
		PreviewPNG: result.Preview,
		Packed:     result.Packed,
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' blob: data:; connect-src 'self'; object-src 'none'; "+
				"base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		writer.Header().Set("Permissions-Policy",
			"bluetooth=(self), camera=(self), geolocation=(), microphone=()")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		writer.Header().Set("Strict-Transport-Security", "max-age=31536000")

		if request.URL.Path == "/" || strings.HasPrefix(request.URL.Path, "/api/") {
			writer.Header().Set("Cache-Control", "no-store")
		} else {
			writer.Header().Set("Cache-Control", "no-cache")
		}

		next.ServeHTTP(writer, request)
	})
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, errorResponse{Error: message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		slog.Warn("write JSON response", "error", err)
	}
}
