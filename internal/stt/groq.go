// Package stt — голос в текст через Groq Whisper (OpenAI-совместимый эндпоинт).
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, filename string) (string, error)
}

type Groq struct {
	apiKey string
	client *http.Client
	URL    string
	Model  string
}

func NewGroq(apiKey string, client *http.Client) *Groq {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Groq{apiKey: apiKey, client: client,
		URL:   "https://api.groq.com/openai/v1/audio/transcriptions",
		Model: "whisper-large-v3-turbo"}
}

func (g *Groq) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	for k, v := range map[string]string{"model": g.Model, "language": "ru", "response_format": "json", "temperature": "0"} {
		if err := w.WriteField(k, v); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("groq %s: %.300s", resp.Status, raw)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}
