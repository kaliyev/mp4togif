package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
)

type ConvertRequest struct {
	FPS        int    `json:"fps"`
	Width      int    `json:"width"`
	Start      int    `json:"start"`
	Duration   int    `json:"duration"`
	S3InputKey string `json:"s3_input_key"`
}

type convertOpts struct {
	FPS      int
	Start    int // seconds, used only when Duration > 0
	Duration int // seconds; 0 means 30s from the middle of the video
}

// convertedKey is the public output naming contract: <basename>_converted.mp4 at the bucket root.
func convertedKey(inputKey string) string {
	base := filepath.Base(inputKey)
	return strings.TrimSuffix(base, filepath.Ext(base)) + "_converted.mp4"
}

// POST /convert: body=JSON -> converts an S3 object and uploads the result to the same bucket
func (c *cfg) handleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request size
	r.Body = http.MaxBytesReader(w, r.Body, c.maxBodyBytes)
	defer r.Body.Close()

	var req ConvertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.S3InputKey == "" {
		http.Error(w, "missing filepath or s3_input_key", http.StatusBadRequest)
		return
	}

	if c.s3Client == nil {
		http.Error(w, "s3 client not configured", http.StatusInternalServerError)
		return
	}

	tmpDir, err := os.MkdirTemp("", "mp4gif-*")
	if err != nil {
		http.Error(w, "tmp dir error", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	inMp4 := filepath.Join(tmpDir, "input.mp4")
	err = c.s3Client.FGetObject(r.Context(), c.s3Bucket, req.S3InputKey, inMp4, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, "failed to download from s3: "+err.Error(), http.StatusInternalServerError)
		return
	}

	outName := convertedKey(req.S3InputKey)
	outMp4 := filepath.Join(tmpDir, outName)

	opts := convertOpts{FPS: req.FPS, Start: req.Start, Duration: req.Duration}
	if err := c.convertVideo(r.Context(), inMp4, outMp4, opts); err != nil {
		http.Error(w, "ffmpeg error: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	if _, err := os.Stat(outMp4); err != nil {
		http.Error(w, "mp4 video file not found", http.StatusNotFound)
		return
	}

	_, err = c.s3Client.FPutObject(r.Context(), c.s3Bucket, outName, outMp4, minio.PutObjectOptions{
		ContentType: "video/mp4",
	})
	if err != nil {
		http.Error(w, "failed to upload to s3: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sha := sha256.Sum256([]byte(req.S3InputKey))
	hash := hex.EncodeToString(sha[:])[:12]
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"success":"ok","path":"` + outName + `","hash":"` + hash + `"}`))
}

// convertVideo trims and re-encodes inPath into outPath with ffmpeg, bounded by cfg.ffmpegTimeout.
func (c *cfg) convertVideo(ctx context.Context, inPath, outPath string, o convertOpts) error {
	fps := clampInt(cmp.Or(o.FPS, 10), 1, 30)

	// Logical cut (seconds)
	var start, dur float64
	if o.Duration > 0 {
		start = float64(o.Start)
		dur = float64(o.Duration)
	} else {
		// Default to 30 seconds from the middle
		totalDur, err := getVideoDuration(ctx, inPath)
		if err != nil {
			log.Printf("failed to get duration for %s: %v", inPath, err)
		}
		const target = 30.0
		if totalDur > target {
			start = (totalDur - target) / 2
			dur = target
		} else {
			start = 0
			dur = totalDur
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.ffmpegTimeout)
	defer cancel()

	// Build common args (optional trim)
	args := []string{"-y"}
	if start > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", start))
	}
	if dur > 0 {
		args = append(args, "-t", fmt.Sprintf("%.3f", dur))
	}

	// Input and encoding parameters
	args = append(args, "-i", inPath, "-r", fmt.Sprintf("%d", fps), "-preset", "medium", "-crf", "28", outPath)

	return runFFmpeg(ctx, args)
}

func getVideoDuration(ctx context.Context, path string) (float64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}

func runFFmpeg(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// Keep logs for debugging but not too noisy; you can redirect to /dev/null if you want.
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timeout")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		// include the last ~2 KB of output
		const max = 2048
		s := string(out)
		if len(s) > max {
			s = s[len(s)-max:]
		}
		return fmt.Errorf("%v; ffmpeg: %s", err, s)
	}
	return nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
