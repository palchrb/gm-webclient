package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

const maxUploadSize = 10 << 20 // 10 MB

// handleSendMedia handles media uploads from the web client.
// Accepts multipart form with:
//   - file: the media file
//   - to: JSON array of recipient phone numbers
//   - conversationId: (optional) existing conversation ID
//   - body: (optional) caption text
func (s *Server) handleSendMedia(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file too large or invalid form"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file is required"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read file"})
		return
	}

	var to []string
	if err := json.Unmarshal([]byte(r.FormValue("to")), &to); err != nil || len(to) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to is required (JSON array of phone numbers)"})
		return
	}

	caption := r.FormValue("body")
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	s.logger.Debug("Media upload received",
		"contentType", contentType,
		"size", len(data),
		"filename", header.Filename,
		"to", to,
	)

	// Determine Garmin media type and convert
	var converted []byte
	var gmMediaType gm.MediaType
	var opts []gm.SendMessageOption

	switch {
	case isImageMIME(contentType):
		gmMediaType = gm.MediaTypeImageAvif
		converted, err = toGarminAVIF(r.Context(), data, contentType)
		if err != nil {
			s.logger.Error("AVIF conversion failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "image conversion failed: " + err.Error()})
			return
		}
		s.logger.Debug("Image converted to AVIF", "originalSize", len(data), "convertedSize", len(converted))

	case isAudioMIME(contentType):
		gmMediaType = gm.MediaTypeAudioOgg
		converted, err = toGarminOGG(r.Context(), data, contentType)
		if err != nil {
			s.logger.Error("OGG conversion failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "audio conversion failed: " + err.Error()})
			return
		}
		// Probe duration for metadata
		if durationMS := probeAudioDurationMS(r.Context(), converted, "ogg"); durationMS > 0 {
			opts = append(opts, gm.WithMediaMetadata(gm.MediaMetadata{DurationMs: &durationMS}))
		}
		s.logger.Debug("Audio converted to OGG", "originalSize", len(data), "convertedSize", len(converted))

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported media type: " + contentType})
		return
	}

	result, err := session.Account.API.SendMediaMessage(r.Context(), to, caption, converted, gmMediaType, opts...)
	if err != nil {
		handleAPIError(w, err, "send media message")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleProxyMedia proxies a media download from Garmin to the browser.
// This avoids CORS issues with Garmin's S3 signed URLs.
// In-memory media cache to avoid repeated Garmin S3 downloads.
// Keyed by mediaId. Entries are small (AVIF images ~50-200KB).
var mediaCache sync.Map // map[string]mediaCacheEntry

type mediaCacheEntry struct {
	data      []byte
	mediaType gm.MediaType
}

func (s *Server) handleProxyMedia(w http.ResponseWriter, r *http.Request) {
	session := getSession(r.Context())

	msgUUID, err := uuid.Parse(r.URL.Query().Get("uuid"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid uuid"})
		return
	}
	mediaID, err := uuid.Parse(r.URL.Query().Get("mediaId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid mediaId"})
		return
	}
	messageID, err := uuid.Parse(r.URL.Query().Get("messageId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid messageId"})
		return
	}
	conversationID, err := uuid.Parse(r.URL.Query().Get("conversationId"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversationId"})
		return
	}
	mediaType := gm.MediaType(r.URL.Query().Get("mediaType"))
	cacheKey := mediaID.String()

	// Check server-side cache first (avoids repeated Garmin S3 downloads)
	if cached, ok := mediaCache.Load(cacheKey); ok {
		entry := cached.(mediaCacheEntry)
		serveMedia(w, entry.data, entry.mediaType)
		return
	}

	data, err := session.Account.API.DownloadMedia(r.Context(), msgUUID, mediaID, messageID, conversationID, mediaType)
	if err != nil {
		handleAPIError(w, err, "download media")
		return
	}

	// Cache for subsequent requests
	mediaCache.Store(cacheKey, mediaCacheEntry{data: data, mediaType: mediaType})

	serveMedia(w, data, mediaType)
}

func serveMedia(w http.ResponseWriter, data []byte, mediaType gm.MediaType) {
	switch mediaType {
	case gm.MediaTypeImageAvif:
		w.Header().Set("Content-Type", "image/avif")
	case gm.MediaTypeAudioOgg:
		w.Header().Set("Content-Type", "audio/ogg")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(data)
}

// ─── ffmpeg conversion (matches reference implementation) ────────────────────

func toGarminAVIF(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
	if _, lookupErr := exec.LookPath("ffmpeg"); lookupErr != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", lookupErr)
	}

	// Write input to temp file so ffmpeg can auto-detect the format
	// (more reliable than guessing demuxer from MIME type)
	tmpIn, err := os.CreateTemp("", "garmin-img-in-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp input: %w", err)
	}
	tmpInPath := tmpIn.Name()
	tmpIn.Write(src)
	tmpIn.Close()
	defer os.Remove(tmpInPath)

	tmpOut, err := os.CreateTemp("", "garmin-avif-*.avif")
	if err != nil {
		return nil, fmt.Errorf("creating temp output: %w", err)
	}
	tmpOutPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpOutPath)

	var errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", tmpInPath,
		"-vf", "scale='min(1920,iw)':'min(1920,ih)':force_original_aspect_ratio=decrease:flags=lanczos",
		"-frames:v", "1",
		"-c:v", "libaom-av1",
		"-crf", "32", "-b:v", "0",
		"-cpu-used", "8",
		"-pix_fmt", "yuv444p",
		"-still-picture", "1",
		"-y", tmpOutPath,
	)
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("image to AVIF: %w\n%s", err, errBuf.String())
	}
	return os.ReadFile(tmpOutPath)
}

func toGarminOGG(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
	if _, lookupErr := exec.LookPath("ffmpeg"); lookupErr != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", lookupErr)
	}

	// Write input to temp file so ffmpeg can auto-detect the input format.
	// Output uses pipe with explicit -f ogg, matching the reference
	// implementation (matrix-garmin-messenger) exactly.
	tmpIn, err := os.CreateTemp("", "garmin-audio-in-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp input: %w", err)
	}
	tmpInPath := tmpIn.Name()
	tmpIn.Write(src)
	tmpIn.Close()
	defer os.Remove(tmpInPath)

	// Output to temp file with explicit -f ogg. Using temp file instead of pipe
	// ensures proper OGG page headers (granule positions, duration) which some
	// players (Garmin iOS app) require for playback.
	tmpOut, err := os.CreateTemp("", "garmin-audio-out-*.ogg")
	if err != nil {
		return nil, fmt.Errorf("creating temp output: %w", err)
	}
	tmpOutPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpOutPath)

	var errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", tmpInPath,
		"-t", "30",
		"-ar", "48000",
		"-ac", "1",
		"-c:a", "libopus",
		"-b:a", "16k",
		"-f", "ogg",
		"-y", tmpOutPath,
	)
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("audio to OGG: %w\n%s", err, errBuf.String())
	}
	return os.ReadFile(tmpOutPath)
}

func isImageMIME(mime string) bool {
	base := strings.Split(mime, ";")[0]
	switch base {
	case "image/jpeg", "image/jpg", "image/png", "image/webp", "image/avif":
		return true
	}
	return false
}

func isAudioMIME(mime string) bool {
	base := strings.Split(mime, ";")[0]
	switch base {
	case "audio/ogg", "audio/mpeg", "audio/mp3", "audio/mp4", "audio/m4a",
		"audio/aac", "audio/wav", "audio/wave", "audio/webm":
		return true
	}
	return false
}

func probeAudioDurationMS(ctx context.Context, data []byte, srcFormat string) int {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-f", srcFormat,
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		"pipe:0",
	)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(f * 1000)
}
