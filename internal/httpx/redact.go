package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const redacted = "[REDACTED]"

// RedactHeaders returns a copy without credential-bearing headers.
func RedactHeaders(headers http.Header) http.Header {
	redactedHeaders := headers.Clone()
	for key := range redactedHeaders {
		if isSecretHeaderKey(key) {
			delete(redactedHeaders, key)
		}
	}
	return redactedHeaders
}

func isSecretHeaderKey(key string) bool {
	key = strings.ToLower(key)
	compact := strings.NewReplacer("-", "", "_", "").Replace(key)
	return key == "authorization" || key == "cookie" || key == "set-cookie" || strings.HasPrefix(key, "x-amz-") || strings.Contains(compact, "token") || strings.Contains(compact, "signature") || strings.Contains(compact, "accesskey") || strings.Contains(compact, "credential")
}

// RedactURL returns a diagnostic URL with sensitive query values removed.
func RedactURL(raw *url.URL) string {
	if raw == nil {
		return ""
	}
	copyURL := *raw
	if copyURL.User != nil {
		copyURL.User = url.User(redacted)
	}
	query := copyURL.Query()
	for key, values := range query {
		if isSecretQueryKey(key) {
			for i := range values {
				values[i] = redacted
			}
			query[key] = values
		}
	}
	copyURL.RawQuery = query.Encode()
	return copyURL.String()
}

// DecodeJSON decodes a single JSON value from resp and closes its body.
func DecodeJSON(resp *http.Response, maxBytes int64, dst any) error {
	if resp == nil || resp.Body == nil {
		return errors.New("response body is missing")
	}
	defer resp.Body.Close()
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponse
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return errors.New("read response body")
	}
	if int64(len(data)) > maxBytes {
		return errors.New("response JSON exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(dst); err != nil {
		return errors.New("decode response JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("response JSON has trailing data")
	}
	return nil
}
