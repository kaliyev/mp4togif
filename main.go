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
	"time"
)

type cfg struct {
	addr          string
	maxBodyBytes  int64
	ffmpegTimeout time.Duration
	videosDir     string
}

type ConvertRequest struct {
	FilePath string `json:"filepath"`
	FPS      int    `json:"fps"`
	Width    int    `json:"width"`
	Start    int    `json:"start"`
	Duration int    `json:"duration"`
}

func main() {
	c := cfg{
		addr:          env("ADDR", ":8180"),
		maxBodyBytes:  envInt64("MAX_BODY_BYTES", 200*1024*1024), // 200MB
		ffmpegTimeout: envDur("FFMPEG_TIMEOUT", 2*time.Minute),
		videosDir:     env("VIDEOS_DIR", "tmp/videos/"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// POST /convert : body=json -> response=gif bytes
	mux.HandleFunc("/convert", func(w http.ResponseWriter, r *http.Request) {
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

		if req.FilePath == "" {
			http.Error(w, "missing filepath", http.StatusBadRequest)
			return
		}

		inMp4 := filepath.Join(c.videosDir, req.FilePath)
		if _, err := os.Stat(inMp4); err != nil {
			http.Error(w, "video file not found", http.StatusNotFound)
			return
		}

		// Read options from JSON
		fps := clampInt(cmp.Or(req.FPS, 10), 1, 30)
		width := clampInt(cmp.Or(req.Width, 480), 64, 1920)

		// Get video duration to calculate middle 30 seconds if needed
		totalDur, err := getVideoDuration(r.Context(), inMp4)
		if err != nil {
			log.Printf("failed to get duration for %s: %v", inMp4, err)
		}

		// Logical cut (seconds)
		var start, dur float64
		if req.Duration > 0 {
			start = float64(req.Start)
			dur = float64(req.Duration)
		} else {
			// Default to 30 seconds from the middle
			const target = 30.0
			if totalDur > target {
				start = (totalDur - target) / 2
				dur = target
			} else {
				start = 0
				dur = totalDur
			}
		}

		tmpDir, err := os.MkdirTemp("", "mp4gif-*")
		if err != nil {
			http.Error(w, "tmp dir error", http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(tmpDir)

		pal := filepath.Join(tmpDir, "palette.png")
		inBase := filepath.Base(req.FilePath)
		ext := filepath.Ext(inBase)
		outName := inBase[:len(inBase)-len(ext)] + ".gif"
		outGif := filepath.Join(tmpDir, outName)

		sha := sha256.New()
		if _, err := os.Stat(inMp4); err == nil {
			// Read a bit of the file to get a hash for the filename hint, or just use the name
			// To be efficient, we can hash the filepath and file info
			sha.Write([]byte(req.FilePath))
		}

		// Run ffmpeg with timeout
		ctx, cancel := context.WithTimeout(r.Context(), c.ffmpegTimeout)
		defer cancel()

		filterPalette := fmt.Sprintf("fps=%d,scale=%d:-1:flags=lanczos,palettegen=stats_mode=diff", fps, width)
		filterGif := fmt.Sprintf("fps=%d,scale=%d:-1:flags=lanczos[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=1:diff_mode=rectangle", fps, width)

		// Build common args (optional trim)
		common := []string{}
		if start > 0 {
			common = append(common, "-ss", fmt.Sprintf("%.3f", start))
		}
		if dur > 0 {
			common = append(common, "-t", fmt.Sprintf("%.3f", dur))
		}

		// 1) palette
		args1 := append([]string{"-y"}, common...)
		args1 = append(args1, "-i", inMp4, "-vf", filterPalette, pal)
		if err := runFFmpeg(ctx, args1); err != nil {
			http.Error(w, "ffmpeg palette error: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}

		// 2) gif
		args2 := append([]string{"-y"}, common...)
		args2 = append(args2, "-i", inMp4, "-i", pal, "-lavfi", filterGif, outGif)
		if err := runFFmpeg(ctx, args2); err != nil {
			http.Error(w, "ffmpeg gif error: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}

		if _, err := os.Stat(outGif); err != nil {
			http.Error(w, "video file not found", http.StatusNotFound)
			return
		}

		finalPath := filepath.Join(c.videosDir, outName)
		if err := os.Rename(outGif, finalPath); err != nil {
			http.Error(w, "failed to move gif: "+err.Error(), http.StatusInternalServerError)
			return
		}

		hash := hex.EncodeToString(sha.Sum(nil))[:12]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":"ok","hash":"` + hash + `"}`))
	})

	s := &http.Server{
		Addr:              c.addr,
		Handler:           logMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("converter listening on %s", c.addr)
	log.Fatal(s.ListenAndServe())
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
	if err != nil {
		// include last ~2KB of output
		const max = 2048
		s := string(out)
		if len(s) > max {
			s = s[len(s)-max:]
		}
		return fmt.Errorf("%v; ffmpeg: %s", err, s)
	}
	return nil
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
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
