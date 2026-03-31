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

	data, err := session.Account.API.DownloadMedia(r.Context(), msgUUID, mediaID, messageID, conversationID, mediaType)
	if err != nil {
		handleAPIError(w, err, "download media")
		return
	}

	switch mediaType {
	case gm.MediaTypeImageAvif:
		w.Header().Set("Content-Type", "image/avif")
	case gm.MediaTypeAudioOgg:
		w.Header().Set("Content-Type", "audio/ogg")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(data)
}

// ─── ffmpeg conversion (matches reference implementation) ────────────────────

func toGarminAVIF(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
	srcFormat, err := mimeToFFmpegFormat(srcMime)
	if err != nil {
		return nil, err
	}
	if _, lookupErr := exec.LookPath("ffmpeg"); lookupErr != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", lookupErr)
	}
	tmpOut, err := os.CreateTemp("", "garmin-avif-*.avif")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpPath)

	var errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", srcFormat, "-i", "pipe:0",
		"-vf", "scale='min(1920,iw)':'min(1920,ih)':force_original_aspect_ratio=decrease:flags=lanczos",
		"-c:v", "libaom-av1",
		"-crf", "30", "-b:v", "0",
		"-cpu-used", "6",
		"-pix_fmt", "yuv444p",
		"-y", tmpPath,
	)
	cmd.Stdin = bytes.NewReader(src)
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("image to AVIF: %w\n%s", err, errBuf.String())
	}
	return os.ReadFile(tmpPath)
}

func toGarminOGG(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
	srcFormat, err := mimeToFFmpegFormat(srcMime)
	if err != nil {
		return nil, err
	}
	if _, lookupErr := exec.LookPath("ffmpeg"); lookupErr != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", lookupErr)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", srcFormat, "-i", "pipe:0",
		"-t", "30",
		"-ar", "48000",
		"-ac", "1",
		"-c:a", "libopus",
		"-b:a", "16k",
		"-f", "ogg", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("audio to OGG: %w\n%s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

func mimeToFFmpegFormat(mime string) (string, error) {
	switch strings.Split(mime, ";")[0] {
	case "image/jpeg", "image/jpg":
		return "mjpeg", nil
	case "image/png":
		return "png_pipe", nil
	case "image/webp":
		return "webp_pipe", nil
	case "image/avif":
		return "avif", nil
	case "audio/ogg":
		return "ogg", nil
	case "audio/mpeg", "audio/mp3":
		return "mp3", nil
	case "audio/mp4", "audio/m4a", "audio/aac":
		return "aac", nil
	case "audio/wav", "audio/wave":
		return "wav", nil
	case "audio/webm":
		return "webm", nil
	default:
		return "", fmt.Errorf("unsupported media type: %s", mime)
	}
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
