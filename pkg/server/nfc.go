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
	encoded := nfcink.EncodeImage(img, cfg)
	if err := device.WriteImageD3(ctx, encoded, 0); err != nil {
		return fmt.Errorf("write nfcink image: %w", err)
	}
	if err := device.Refresh(ctx, 0); err != nil {
		return fmt.Errorf("refresh nfcink badge: %w", err)
	}
	return nil
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
	Probe(ctx context.Context) (nfcReaderStatus, error)
}

// pcscReaderProbe is the real ACR1552/PC/SC liveness check.
type pcscReaderProbe struct{}

func (pcscReaderProbe) Probe(ctx context.Context) (nfcReaderStatus, error) {
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
	badgeCtx, cancel := context.WithTimeout(ctx, nfcBadgeProbeBudget)
	defer cancel()
	card, err := session.WaitForCard(badgeCtx, reader, 100*time.Millisecond)
	if err != nil {
		return nfcReaderStatus{Reader: reader}, nil
	}
	card.Close()
	return nfcReaderStatus{Reader: reader, BadgePresent: true}, nil
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

	status, err := service.nfcProbe.Probe(ctx)
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
		emit("error", 0, err.Error())
		return
	}
	emit("done", 100, "Badge write completed.")
}
