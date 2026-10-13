package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// telegram is a minimal Bot API client. A zero value (empty token) disables sending.
type telegram struct {
	api    string
	token  string
	chatID string
	client *http.Client
}

func newTelegram(api, token, chatID string) *telegram {
	return &telegram{api: strings.TrimSuffix(api, "/"), token: token, chatID: chatID, client: &http.Client{Timeout: 2 * time.Minute}}
}

func (t *telegram) enabled() bool { return t.token != "" && t.chatID != "" }

func (t *telegram) sendMessage(ctx context.Context, html string) error {
	form := url.Values{"chat_id": {t.chatID}, "text": {html}, "parse_mode": {"HTML"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint("sendMessage"), bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return t.do(req)
}

// sendVideo uploads a local file. Bot API rejects uploads over 50 MB.
func (t *telegram) sendVideo(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if err := mw.WriteField("chat_id", t.chatID); err != nil {
				return err
			}
			if err := mw.WriteField("supports_streaming", "true"); err != nil {
				return err
			}
			part, err := mw.CreateFormFile("video", filepath.Base(path))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint("sendVideo"), pr)
	if err != nil {
		pr.CloseWithError(err)
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return t.do(req)
}

func (t *telegram) endpoint(method string) string {
	return t.api + "/bot" + t.token + "/" + method
}

func (t *telegram) do(req *http.Request) error {
	resp, err := t.client.Do(req)
	if err != nil {
		// url.Error includes the request URL, which contains the bot token
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err
		}
		return fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body)
	if resp.StatusCode != http.StatusOK || !body.OK {
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode, body.Description)
	}
	return nil
}
