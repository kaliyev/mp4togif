package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

// Ported from the n8n "SchoolIncidents" workflow.
var incidentRequired = []string{"location", "time", "date"}

// Conversion settings the n8n workflow passed to /convert.
var incidentConvert = convertOpts{FPS: 5, Start: 0, Duration: 30}

const (
	incidentVideoField = "video"
	maxFormTextBytes   = 1 << 20 // all text fields together; the video goes to disk
)

type incidentJob struct {
	id        string
	tmpDir    string
	inputPath string
	key       string // S3 key of the original video
}

type incidents struct {
	c    *cfg
	db   *pgxpool.Pool
	tg   *telegram
	jobs chan incidentJob
	wg   sync.WaitGroup

	stop       chan struct{}
	procCtx    context.Context
	cancelProc context.CancelFunc
}

func newIncidents(c *cfg, db *pgxpool.Pool, tg *telegram) *incidents {
	procCtx, cancel := context.WithCancel(context.Background())
	return &incidents{
		c: c, db: db, tg: tg,
		jobs: make(chan incidentJob, c.ingestQueueSize),
		stop: make(chan struct{}), procCtx: procCtx, cancelProc: cancel,
	}
}

// POST <INGEST_PATH>: multipart form with incident fields and a "video" file.
// Stores the original in S3, inserts a core_incident row, responds, then converts and notifies in the background.
func (s *incidents) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if reason := s.authError(r); reason != "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="incidents"`)
		writeResult(w, http.StatusUnauthorized, reason)
		return
	}
	if s.c.s3Client == nil {
		log.Printf("incident: s3 client not configured")
		writeResult(w, http.StatusInternalServerError, "something wrong")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.c.maxBodyBytes)
	defer r.Body.Close()

	tmpDir, err := os.MkdirTemp("", "incident-*")
	if err != nil {
		log.Printf("incident: tmp dir: %v", err)
		writeResult(w, http.StatusInternalServerError, "something wrong")
		return
	}
	queued := false
	defer func() {
		if !queued {
			os.RemoveAll(tmpDir)
		}
	}()

	form, err := readIncidentForm(r, tmpDir)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeResult(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		writeResult(w, http.StatusBadRequest, err.Error())
		return
	}

	body := form.fields
	var missing []string
	for _, k := range incidentRequired {
		if strings.TrimSpace(body[k]) == "" {
			missing = append(missing, k)
		}
	}
	if form.videoPath == "" {
		missing = append(missing, incidentVideoField)
	}
	if len(missing) > 0 {
		writeResult(w, http.StatusBadRequest, "fields missing: "+strings.Join(missing, ", "))
		return
	}

	id := uuid.NewString()
	key := id + form.videoExt
	ctx := r.Context()

	_, err = s.c.s3Client.FPutObject(ctx, s.c.s3Bucket, key, form.videoPath, minio.PutObjectOptions{ContentType: form.videoType})
	if err != nil {
		log.Printf("incident %s: upload original: %v", id, err)
		writeResult(w, http.StatusInternalServerError, "something wrong")
		return
	}

	if err := s.insert(ctx, id, body, key); err != nil {
		log.Printf("incident %s: insert: %v", id, err)
		if err := s.c.s3Client.RemoveObject(context.WithoutCancel(ctx), s.c.s3Bucket, key, minio.RemoveObjectOptions{}); err != nil {
			log.Printf("incident %s: remove orphaned %s: %v", id, key, err)
		}
		writeResult(w, http.StatusInternalServerError, "something wrong")
		return
	}
	log.Printf("incident %s: stored, original %q -> %s", id, form.videoName, key)

	if s.tg.enabled() {
		s.wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.tg.sendMessage(ctx, s.message(body)); err != nil {
				log.Printf("incident %s: telegram message: %v", id, err)
			}
		})
	}

	select {
	case s.jobs <- incidentJob{id: id, tmpDir: tmpDir, inputPath: form.videoPath, key: key}:
		queued = true
	case <-ctx.Done():
		log.Printf("incident %s: client gone before conversion was queued, skipping conversion", id)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "success", "id": id})
}

// authError checks basic auth and returns why it failed, or "" when the request is authorized.
func (s *incidents) authError(r *http.Request) string {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return "missing credentials"
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.c.ingestUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.c.ingestPassword)) == 1
	if !userOK || !passOK {
		return "invalid credentials"
	}
	return ""
}

type incidentForm struct {
	fields    map[string]string
	videoPath string
	videoName string
	videoExt  string
	videoType string
}

// readIncidentForm streams the multipart body: the video part goes straight to tmpDir, other parts are read as text.
func readIncidentForm(r *http.Request, tmpDir string) (*incidentForm, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, errors.New("expected multipart/form-data")
	}
	form := &incidentForm{fields: map[string]string{}}
	textLeft := int64(maxFormTextBytes)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return form, nil
		}
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		if name == incidentVideoField && part.FileName() != "" {
			if form.videoPath != "" {
				return nil, errors.New("more than one video")
			}
			ext := strings.ToLower(filepath.Ext(part.FileName()))
			if ext == "" {
				ext = ".mp4"
			}
			path := filepath.Join(tmpDir, "input"+ext)
			if err := saveTo(path, part); err != nil {
				return nil, err
			}
			form.videoPath, form.videoName, form.videoExt = path, part.FileName(), ext
			form.videoType = part.Header.Get("Content-Type")
			if form.videoType == "" || form.videoType == "application/octet-stream" {
				form.videoType = "video/mp4"
			}
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, textLeft+1))
		if err != nil {
			return nil, err
		}
		textLeft -= int64(len(b))
		if textLeft < 0 {
			return nil, errors.New("form fields too large")
		}
		form.fields[name] = string(b)
	}
}

func saveTo(path string, src io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// insert stores the form fields as core_incident.data, like the n8n query did.
func (s *incidents) insert(ctx context.Context, id string, body map[string]string, key string) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// date arrives as DD.MM.YYYY; Postgres parses "YYYY-MM-DD <time>" in the session time zone, same as the n8n query.
	parts := strings.Split(body["date"], ".")
	slices.Reverse(parts)
	datetime := strings.Join(parts, "-") + " " + body["time"]

	_, err = s.db.Exec(ctx, `
		INSERT INTO core_incident (id, location, datetime, data, video, school_id, created_at)
		VALUES ($1::text::uuid, $2, $3::text::timestamptz, $4::text::jsonb, $5, $6::text::uuid, now())`,
		id, body["location"], datetime, string(data), key, s.c.schoolID)
	return err
}

func (s *incidents) message(body map[string]string) string {
	f := func(k string) string { return html.EscapeString(body[k]) }
	return fmt.Sprintf("%s\nКлассификация: <b>%s</b>\n\nМесто: <b>%s</b>\nОписание: <i>%s</i>\nПроцент уверенности: %s\nОценка буллинга: %s\nОценка драки: %s\nДата: %s - %s",
		html.EscapeString(s.c.telegramHeader),
		f("classification"), f("location"), f("description"),
		f("confidence_percentage"), f("bullying_score"), f("fight_score"),
		f("date"), f("time"))
}

// run starts the conversion workers; they take jobs until shutdown is called.
func (s *incidents) run() {
	for range s.c.ingestWorkers {
		s.wg.Go(func() {
			for {
				select {
				case <-s.stop:
					return
				default:
				}
				select {
				case <-s.stop:
					return
				case j := <-s.jobs:
					s.process(s.procCtx, j)
				}
			}
		})
	}
}

// shutdown stops taking jobs, lets running conversions finish until ctx expires, then cancels them.
// Jobs still queued are logged and dropped; their originals stay in S3.
func (s *incidents) shutdown(ctx context.Context) {
	close(s.stop)
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("incident: shutdown timeout, cancelling running conversions")
		s.cancelProc()
		<-done
	}
	for {
		select {
		case j := <-s.jobs:
			log.Printf("incident %s: conversion dropped on shutdown, original at %s", j.id, j.key)
			os.RemoveAll(j.tmpDir)
		default:
			return
		}
	}
}

func (s *incidents) process(ctx context.Context, j incidentJob) {
	defer os.RemoveAll(j.tmpDir)
	start := time.Now()

	outKey := convertedKey(j.key)
	outPath := filepath.Join(j.tmpDir, outKey)
	if err := s.c.convertVideo(ctx, j.inputPath, outPath, incidentConvert); err != nil {
		log.Printf("incident %s: convert: %v", j.id, err)
		return
	}
	if _, err := s.c.s3Client.FPutObject(ctx, s.c.s3Bucket, outKey, outPath, minio.PutObjectOptions{ContentType: "video/mp4"}); err != nil {
		log.Printf("incident %s: upload converted: %v", j.id, err)
		return
	}
	if s.tg.enabled() {
		if err := s.tg.sendVideo(ctx, outPath); err != nil {
			log.Printf("incident %s: telegram video: %v", j.id, err)
		}
	}
	log.Printf("incident %s: converted to %s in %s", j.id, outKey, time.Since(start).Round(time.Millisecond))
}

func writeResult(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "error", "message": message})
}
