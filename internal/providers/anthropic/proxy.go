package anthropic

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

type Upstream struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
	APIVer  string
	Timeout time.Duration
}

func DoMessages(ctx context.Context, up Upstream, body []byte) (*http.Response, error) {
	url := buildMessagesURL(up.BaseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(up.APIVer) != "" {
		req.Header.Set("anthropic-version", up.APIVer)
	}
	if strings.TrimSpace(up.APIKey) != "" {
		req.Header.Set("x-api-key", up.APIKey)
	}
	for k, v := range up.Headers {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	client := &http.Client{}
	return client.Do(req)
}

func DoModels(ctx context.Context, up Upstream) (*http.Response, error) {
	url := buildModelsURL(up.BaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(up.APIVer) != "" {
		req.Header.Set("anthropic-version", up.APIVer)
	}
	if strings.TrimSpace(up.APIKey) != "" {
		req.Header.Set("x-api-key", up.APIKey)
	}
	for k, v := range up.Headers {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	client := &http.Client{}
	return client.Do(req)
}

func CopySSE(w http.ResponseWriter, r io.Reader) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, err := io.Copy(w, r)
		return err
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func buildMessagesURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

func buildModelsURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}
