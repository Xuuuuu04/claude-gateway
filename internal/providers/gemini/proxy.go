package gemini

import (
	"bytes"
	"context"
	"net/http"
	"strings"
)

type Upstream struct {
	BaseURL string
	APIKey  string
	Headers map[string]string
}

func DoGenerateContent(ctx context.Context, up Upstream, model string, body []byte) (*http.Response, error) {
	url := buildURL(up.BaseURL, "/v1beta/models/"+model+":generateContent")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(up.APIKey) != "" {
		req.Header.Set("x-goog-api-key", strings.TrimSpace(up.APIKey))
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
	if strings.HasSuffix(base, "/v1beta") {
		return base + strings.TrimPrefix(path, "/v1beta")
	}
	return base + path
}

