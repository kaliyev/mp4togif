package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type cfg struct {
	addr            string
	maxBodyBytes    int64
	ffmpegTimeout   time.Duration
	shutdownTimeout time.Duration
	videosDir       string

	s3Endpoint  string
	s3AccessKey string
	s3SecretKey string
	s3Bucket    string
	s3UseSSL    bool
	s3Client    *minio.Client

	databaseURL     string
	schoolID        string
	ingestPaths     []string
	ingestUser      string
	ingestPassword  string
	ingestWorkers   int
	ingestQueueSize int

	telegramAPI    string
	telegramToken  string
	telegramChatID string
	telegramHeader string
}

func main() {
	c := cfg{
		addr:            env("ADDR", ":8180"),
		maxBodyBytes:    envInt64("MAX_BODY_BYTES", 200*1024*1024), // 200MB
		ffmpegTimeout:   envDur("FFMPEG_TIMEOUT", 2*time.Minute),
		shutdownTimeout: envDur("SHUTDOWN_TIMEOUT", 25*time.Second),
		videosDir:       env("VIDEOS_DIR", "tmp/videos/"),

		s3Endpoint:  env("S3_ENDPOINT", ""),
		s3AccessKey: env("S3_ACCESS_KEY", ""),
		s3SecretKey: env("S3_SECRET_KEY", ""),
		s3Bucket:    env("S3_BUCKET", "videos"),
		s3UseSSL:    env("S3_USE_SSL", "false") == "true",

		databaseURL:     env("DATABASE_URL", ""),
		schoolID:        env("SCHOOL_ID", ""),
		ingestPaths:     envList("INGEST_PATH", "/incidents"),
		ingestUser:      env("INGEST_USER", ""),
		ingestPassword:  env("INGEST_PASSWORD", ""),
		ingestWorkers:   int(max(envInt64("INGEST_WORKERS", 2), 1)),
		ingestQueueSize: int(max(envInt64("INGEST_QUEUE_SIZE", 100), 0)),

		telegramAPI:    env("TELEGRAM_API_URL", "https://api.telegram.org"),
		telegramToken:  env("TELEGRAM_BOT_TOKEN", ""),
		telegramChatID: env("TELEGRAM_CHAT_ID", ""),
		telegramHeader: env("TELEGRAM_HEADER", "Что-то произошло в школе"),
	}

	if c.s3Endpoint != "" {
		minioClient, err := minio.New(c.s3Endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(c.s3AccessKey, c.s3SecretKey, ""),
			Secure: c.s3UseSSL,
		})
		if err != nil {
			log.Fatalf("failed to initialize s3 client: %v", err)
		}
		c.s3Client = minioClient
		log.Printf("s3 client initialized for %s", c.s3Endpoint)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/convert", c.handleConvert)
	mux.HandleFunc("/file", c.handleFile)

	var inc *incidents
	if err := c.ingestConfigError(); err != nil {
		log.Printf("incident ingest disabled: %v", err)
	} else {
		db, err := pgxpool.New(ctx, c.databaseURL)
		if err != nil {
			log.Fatalf("invalid DATABASE_URL: %v", err)
		}
		defer db.Close()
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := db.Ping(pingCtx); err != nil {
			log.Printf("warning: database not reachable yet: %v", err)
		}
		cancel()

		tg := newTelegram(c.telegramAPI, c.telegramToken, c.telegramChatID)
		if !tg.enabled() {
			log.Printf("telegram notifications disabled: TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID not set")
		}
		inc = newIncidents(&c, db, tg)
		inc.run()
		for _, p := range c.ingestPaths {
			mux.HandleFunc(p, inc.handle)
		}
		log.Printf("incident ingest on %s (%d workers)", strings.Join(c.ingestPaths, ", "), c.ingestWorkers)
	}

	s := &http.Server{
		Addr:              c.addr,
		Handler:           logMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), c.shutdownTimeout)
		defer cancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
		if inc != nil {
			inc.shutdown(shutdownCtx)
		}
	}()

	log.Printf("converter listening on %s", c.addr)
	if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

func (c *cfg) ingestConfigError() error {
	var missing []string
	for name, v := range map[string]string{
		"DATABASE_URL": c.databaseURL, "SCHOOL_ID": c.schoolID,
		"INGEST_USER": c.ingestUser, "INGEST_PASSWORD": c.ingestPassword,
		"S3_ENDPOINT": c.s3Endpoint,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("not set: %s", strings.Join(missing, ", "))
	}
	return nil
}

// GET /file?key=...: downloads file from s3
func (c *cfg) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}

	if c.s3Client == nil {
		http.Error(w, "s3 client not configured", http.StatusInternalServerError)
		return
	}

	obj, err := c.s3Client.GetObject(r.Context(), c.s3Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, "failed to get object from s3: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer obj.Close()

	info, err := obj.Stat()
	if err != nil {
		// Check if object exists
		if errResp, ok := err.(minio.ErrorResponse); ok && errResp.Code == "NoSuchKey" {
			http.Error(w, "file not found", http.StatusNotFound)
		} else {
			http.Error(w, "failed to stat object: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(key)))

	http.ServeContent(w, r, filepath.Base(key), info.LastModified, obj)
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

// envList splits a comma-separated value, dropping empty items.
func envList(key, def string) []string {
	var out []string
	for _, v := range strings.Split(env(key, def), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
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
