package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"webbadge/pkg/badgeimage"
	"webbadge/pkg/nfcbadge"
	"webbadge/pkg/nfcink"
	"webbadge/pkg/pcsc"
)

const (
	maxNFCRequestBytes  = 4 << 20
	nfcCardWaitTimeout  = 30 * time.Second
	nfcWriteBudget      = 2 * time.Minute
	nfcStatusBudget     = 2 * time.Second
	nfcBadgeProbeBudget = 400 * time.Millisecond
	nfcInkProbeBudget   = 1200 * time.Millisecond
)

type nfcEvent struct {
	Stage   string `json:"stage"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
}

// nfcBadgeWriter waits for a badge on an NFC reader and writes an image to
// it, reporting stage/percent/message progress as it goes. It exists so the
// HTTP/SSE plumbing in writeNFC can be tested without real PC/SC hardware.
type nfcBadgeWriter interface {
	Write(ctx context.Context, img image.Image, profile badgeimage.Profile, report func(stage string, percent int, message string)) error
}

// pcscNFCWriter writes either supported badge profile through a local
// ACR1552 PC/SC reader, dispatching to the matching protocol package.
type pcscNFCWriter struct{}

func (pcscNFCWriter) Write(ctx context.Context, img image.Image, profile badgeimage.Profile, report func(stage string, percent int, message string)) error {
	report("connecting", 0, "Opening PC/SC session…")
	session, err := pcsc.Establish()
	if err != nil {
		return fmt.Errorf("establish PC/SC session: %w", err)
	}
	defer session.Close()

	reader, err := session.SelectReader("")
	if err != nil {
		return fmt.Errorf("select PC/SC reader: %w", err)
	}

	report("waiting", 0, fmt.Sprintf("Waiting for badge on %s…", reader))
	waitCtx, cancel := context.WithTimeout(ctx, nfcCardWaitTimeout)
	card, err := session.WaitForCard(waitCtx, reader, 200*time.Millisecond)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for badge: %w", err)
	}
	defer card.Close()

	report("writing", 0, "Writing image to badge…")
	progress := func(completed, total int) {
		percent := 100
		if total > 0 {
			percent = completed * 100 / total
		}
		report("writing", percent, fmt.Sprintf("Writing to badge… %d%%", percent))
	}

	switch profile.ID {
	case badgeimage.Hybrid397.ID:
		writer := nfcbadge.NewNFCA397Writer(card, progress)
		if err := writer.Write(ctx, img); err != nil {
			return fmt.Errorf("write badge: %w", err)
		}
		return nil
	case badgeimage.NfcInk240x416.ID:
		return writeNfcInk(ctx, card, img, progress)
	default:
		return fmt.Errorf("unsupported badge profile %q", profile.ID)
	}
}

// writeNfcInk reads the badge's own reported configuration (geometry,
// colour wiring), encodes img to match it, uploads it via the
// LZO-compressed D3 path, and triggers the screen refresh.
func writeNfcInk(ctx context.Context, card *pcsc.Card, img image.Image, progress func(completed, total int)) error {
	device := nfcink.NewNfcInkDevice(card, progress)
	cfg, err := device.ReadConfig(ctx)
	if err != nil {
		return fmt.Errorf("read nfcink config: %w", err)
	}
	slog.Debug("nfcink write starting",
		"reader", card.Reader(),
		"atr", fmt.Sprintf("%X", card.ATR()),
		"width", cfg.ScreenWidth,
		"height", cfg.ScreenHeight,
		"colors", cfg.ColorCount,
		"picture_capacity", cfg.PictureCapacity,
		"palette", cfg.ColorDic)
	encoded := nfcink.EncodeImage(img, cfg)
	slog.Debug("nfcink image encoded",
		"bytes", len(encoded),
		"preview", summarizeNFCBytes(encoded, 16))
	if err := device.WriteImageD3(ctx, encoded, 0); err != nil {
		return fmt.Errorf("write nfcink image: %w", err)
	}
	if err := device.Refresh(ctx, 0); err != nil {
		return fmt.Errorf("refresh nfcink badge: %w", err)
	}
	return nil
}

func summarizeNFCBytes(data []byte, preview int) string {
	if len(data) == 0 {
		return "len=0"
	}
	if preview <= 0 {
		preview = 1
	}
	if len(data) <= preview {
		return fmt.Sprintf("len=%d hex=%X", len(data), data)
	}
	return fmt.Sprintf("len=%d head=%X tail=%X", len(data), data[:preview], data[len(data)-preview:])
}

// nfcReaderStatus is what a liveness probe found on the local NFC reader.
type nfcReaderStatus struct {
	Reader       string
	BadgePresent bool
}

// nfcReaderProbe checks whether a local NFC reader (and optionally a badge
// on it) is currently reachable, without performing a write. It exists so
// the status handler can be tested without real PC/SC hardware.
type nfcReaderProbe interface {
	Probe(ctx context.Context, profile badgeimage.Profile) (nfcReaderStatus, error)
}

// pcscReaderProbe is the real ACR1552/PC/SC liveness check.
type pcscReaderProbe struct{}

func (pcscReaderProbe) Probe(ctx context.Context, profile badgeimage.Profile) (nfcReaderStatus, error) {
	session, err := pcsc.Establish()
	if err != nil {
		return nfcReaderStatus{}, fmt.Errorf("establish PC/SC session: %w", err)
	}
	defer session.Close()

	reader, err := session.SelectReader("")
	if err != nil {
		return nfcReaderStatus{}, fmt.Errorf("select PC/SC reader: %w", err)
	}

	// A badge not answering within this short window just means there's no
	// badge on the reader right now, which isn't an error worth surfacing.
	probeBudget := nfcBadgeProbeBudget
	if profile.ID == badgeimage.NfcInk240x416.ID {
		probeBudget = nfcInkProbeBudget
	}
	badgeCtx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	card, err := session.WaitForCard(badgeCtx, reader, 100*time.Millisecond)
	if err != nil {
		return nfcReaderStatus{Reader: reader}, nil
	}
	defer card.Close()

	present, err := probeBadgeProfile(badgeCtx, card, profile)
	if err != nil {
		return nfcReaderStatus{Reader: reader}, nil
	}
	return nfcReaderStatus{Reader: reader, BadgePresent: present}, nil
}

func probeBadgeProfile(ctx context.Context, card *pcsc.Card, profile badgeimage.Profile) (bool, error) {
	switch profile.ID {
	case "", badgeimage.Hybrid397.ID:
		return true, nil
	case badgeimage.NfcInk240x416.ID:
		_, err := nfcink.NewNfcInkDevice(card, nil).ReadConfig(ctx)
		if err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported badge profile %q", profile.ID)
	}
}

type nfcStatusResponse struct {
	Available    bool   `json:"available"`
	Reader       string `json:"reader,omitempty"`
	BadgePresent bool   `json:"badgePresent"`
	Error        string `json:"error,omitempty"`
}

func (service *api) nfcStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), nfcStatusBudget)
	defer cancel()

	profile := badgeimage.Hybrid397
	if profileID := request.URL.Query().Get("profile"); profileID != "" {
		var ok bool
		profile, ok = badgeimage.ProfileByID(profileID)
		if !ok {
			writeError(writer, http.StatusBadRequest, "unknown badge profile")
			return
		}
	}

	status, err := service.nfcProbe.Probe(ctx, profile)
	if err != nil {
		writeJSON(writer, http.StatusOK, nfcStatusResponse{Available: false, Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, nfcStatusResponse{
		Available:    true,
		Reader:       status.Reader,
		BadgePresent: status.BadgePresent,
	})
}

func (service *api) writeNFC(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxNFCRequestBytes)
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(writer, http.StatusRequestEntityTooLarge, "image is too large")
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

	profile, ok := badgeimage.ProfileByID(request.FormValue("profile"))
	if !ok {
		writeError(writer, http.StatusBadRequest, "unknown badge profile")
		return
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

	img, err := png.Decode(file)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "image must be a valid PNG")
		return
	}
	bounds := img.Bounds()
	if bounds.Dx() != profile.Width || bounds.Dy() != profile.Height {
		writeError(writer, http.StatusBadRequest,
			fmt.Sprintf("image must be exactly %dx%d pixels", profile.Width, profile.Height))
		return
	}

	select {
	case service.nfcSlots <- struct{}{}:
		defer func() { <-service.nfcSlots }()
	case <-request.Context().Done():
		return
	}

	// Extend only this response's write deadline rather than the server-wide
	// WriteTimeout, since a badge write can legitimately take longer than the
	// timeout that's appropriate for every other endpoint.
	if err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(nfcWriteBudget)); err != nil {
		slog.Warn("extend NFC write deadline", "error", err)
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	flusher, canFlush := writer.(http.Flusher)

	emit := func(stage string, percent int, message string) {
		payload, err := json.Marshal(nfcEvent{Stage: stage, Percent: percent, Message: message})
		if err != nil {
			slog.Warn("encode NFC progress event", "error", err)
			return
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", payload); err != nil {
			slog.Warn("write NFC progress event", "error", err)
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}

	ctx, cancel := context.WithTimeout(request.Context(), nfcWriteBudget)
	defer cancel()

	if err := service.nfcWriter.Write(ctx, img, profile, emit); err != nil {
		slog.Info("NFC badge write failed", "error", err)
		emit("error", 0, userFacingNFCWriteError(profile, err))
		return
	}
	emit("done", 100, "Badge write completed.")
}

func userFacingNFCWriteError(profile badgeimage.Profile, err error) string {
	if message, ok := classifyWrongNFCAreaError(profile, err); ok {
		return message
	}
	return err.Error()
}

func classifyWrongNFCAreaError(profile badgeimage.Profile, err error) (string, bool) {
	message := err.Error()
	switch profile.ID {
	case badgeimage.Hybrid397.ID:
		if strings.Contains(message, "ACR1552 transparent exchange failed") ||
			strings.Contains(message, "wake badge: write FIFO") {
			return "The badge was detected, but the reader reached its regular NFC tag area instead of the e-ink image-writing interface. Reposition the badge and try again.", true
		}
	case badgeimage.NfcInk240x416.ID:
		if strings.Contains(message, "read nfcink config:") ||
			strings.Contains(message, "write nfcink image:") ||
			strings.Contains(message, "refresh nfcink badge:") {
			return "The badge was detected, but it did not answer like the nfcink image-writing interface. If this badge also exposes normal NFC tag data, place the image-writing area on the reader and try again.", true
		}
	}
	return "", false
}
