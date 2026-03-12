package connector

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	gm "github.com/yourusername/matrix-garmin-messenger/internal/hermes"
)

// ToAVIF converts an image (JPEG or PNG) to AVIF for sending to Garmin.
// Garmin Messenger only accepts AVIF for image attachments.
func ToAVIF(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
	srcFormat, err := mimeToFFmpegFormat(srcMime)
	if err != nil {
		return nil, err
	}
	// avif muxer + libaom-av1 encoder. -crf 35 is a good quality/size balance.
	if _, lookupErr := exec.LookPath("ffmpeg"); lookupErr != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", lookupErr)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", srcFormat, "-i", "pipe:0",
		"-c:v", "libaom-av1", "-crf", "35", "-b:v", "0",
		"-f", "avif", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("image→avif: %w\n%s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// ToOGG converts audio (mp3, m4a, wav, etc.) to OGG Vorbis for sending to Garmin.
// Garmin Messenger only accepts OGG for audio attachments.
// Audio is trimmed to max 30 seconds to match Garmin voice-message limits.
func ToOGG(ctx context.Context, src []byte, srcMime string) ([]byte, error) {
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
		"-c:a", "libvorbis", "-q:a", "4",
		"-f", "ogg", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("audio→ogg: %w\n%s", err, errBuf.String())
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

// GarminMediaType returns the Garmin API media type for a given source MIME type.
//
// Garmin only accepts ImageAvif for images and AudioOgg for audio.
func GarminMediaType(mime string) gm.MediaType {
	switch mime {
	case "image/jpeg", "image/jpg", "image/png", "image/webp", "image/avif":
		return gm.MediaTypeImageAvif
	case "audio/ogg", "audio/mpeg", "audio/mp3", "audio/mp4", "audio/m4a",
		"audio/aac", "audio/wav", "audio/wave", "audio/webm":
		return gm.MediaTypeAudioOgg
	default:
		return ""
	}
}
