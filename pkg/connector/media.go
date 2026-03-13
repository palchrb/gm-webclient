package connector

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// ToGarminAVIF converts an image to AVIF matching the iOS Garmin Messenger
// encoding parameters observed from the app's debug menu:
//
//	resolution: 1920 (long edge), quality: 20/100, speed: 6, pixel format: YUV444
//
// quality 20/100 on libavif scale maps to approximately CRF 50 for libaom-av1.
// cpu-used 6 balances encoding speed vs file size (app uses speed=6 on 0–10 scale).
func ToGarminAVIF(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
	srcFormat, err := mimeToFFmpegFormat(srcMime)
	if err != nil {
		return nil, err
	}
	if _, lookupErr := exec.LookPath("ffmpeg"); lookupErr != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", lookupErr)
	}
	// Scale so the longest side is at most 1920px, keeping aspect ratio.
	// CRF 50 matches quality=20/100 on libavif/libaom-av1.
	// yuv444p matches the app's YUV444 pixel format setting.
	// cpu-used 6 for fast encoding (app uses speed=6).
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", srcFormat, "-i", "pipe:0",
		"-vf", "scale='min(1920,iw)':'min(1920,ih)':force_original_aspect_ratio=decrease:flags=lanczos",
		"-c:v", "libaom-av1",
		"-crf", "50", "-b:v", "0",
		"-cpu-used", "6",
		"-pix_fmt", "yuv444p",
		"-f", "avif", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("image→garmin-avif: %w\n%s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// ToGarminOGG converts audio to OGG Vorbis matching the Garmin Messenger
// voice message format: 8000 Hz sample rate, mono, max 30 seconds.
func ToGarminOGG(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
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
		"-t", "30", // max 30 seconds
		"-ar", "8000", // 8000 Hz sample rate (telephone quality)
		"-ac", "1", // mono
		"-c:a", "libvorbis",
		"-q:a", "1", // low quality appropriate for 8kHz voice
		"-f", "ogg", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("audio→garmin-ogg: %w\n%s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// mimeToFFmpegFormat maps common MIME types to ffmpeg demuxer names.
func mimeToFFmpegFormat(mime string) (string, error) {
	switch mime {
	case "image/jpeg", "image/jpg":
		return "mjpeg", nil
	case "image/png":
		return "png_pipe", nil
	case "image/webp":
		return "webp_pipe", nil
	case "image/avif":
		return "avif", nil
	case "audio/ogg", "audio/ogg; codecs=vorbis":
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

// isImageMIME reports whether the MIME type is a supported image format.
func isImageMIME(mime string) bool {
	switch mime {
	case "image/jpeg", "image/jpg", "image/png", "image/webp", "image/avif":
		return true
	}
	return false
}

// isAudioMIME reports whether the MIME type is a supported audio format.
func isAudioMIME(mime string) bool {
	switch mime {
	case "audio/ogg", "audio/ogg; codecs=vorbis",
		"audio/mpeg", "audio/mp3",
		"audio/mp4", "audio/m4a", "audio/aac",
		"audio/wav", "audio/wave",
		"audio/webm":
		return true
	}
	return false
}
