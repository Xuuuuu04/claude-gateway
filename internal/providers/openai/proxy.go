package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
)

type Upstream struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
}

func DoChatCompletions(ctx context.Context, up Upstream, body []byte) (*http.Response, error) {
	return do(ctx, up, buildURL(up.BaseURL, "/v1/chat/completions"), body)
}

func DoResponses(ctx context.Context, up Upstream, body []byte) (*http.Response, error) {
	return do(ctx, up, buildURL(up.BaseURL, "/v1/responses"), body)
}

func DoModels(ctx context.Context, up Upstream) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, buildURL(up.BaseURL, "/v1/models"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(up.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(up.APIKey))
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

func do(ctx context.Context, up Upstream, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(up.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(up.APIKey))
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

func buildURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + strings.TrimPrefix(path, "/v1")
	}
	return base + path
}
