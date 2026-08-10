// Package output renders stable human and JSON command output.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

type Renderer struct {
	Stdout io.Writer
	Stderr io.Writer
	JSON   bool
}

func NewRenderer(stdout, stderr io.Writer, jsonMode bool) *Renderer {
	return &Renderer{Stdout: stdout, Stderr: stderr, JSON: jsonMode}
}

func (r *Renderer) Success(operation string, fields map[string]any, human string) error {
	if !r.JSON {
		_, err := fmt.Fprintln(r.Stdout, human)
		return err
	}
	payload := map[string]any{"ok": true, "operation": operation}
	for key, value := range fields {
		if key == "ok" || key == "operation" {
			continue
		}
		payload[key] = redactValue(key, value)
	}
	return json.NewEncoder(r.Stdout).Encode(payload)
}

func (r *Renderer) Failure(category, message string) error {
	message = RedactMessage(message)
	if r.JSON {
		message = redactAllURLs(message)
		return json.NewEncoder(r.Stdout).Encode(map[string]any{"ok": false, "category": category, "message": message})
	}
	_, err := fmt.Fprintln(r.Stderr, message)
	return err
}

func redactValue(key string, value any) any {
	if sensitiveKey(key) {
		return "[REDACTED]"
	}
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[REDACTED]"
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return "[REDACTED]"
	}
	return redactGeneric(generic)
}

func redactGeneric(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, nested := range typed {
			if sensitiveKey(key) {
				clean[key] = "[REDACTED]"
			} else {
				clean[key] = redactGeneric(nested)
			}
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, nested := range typed {
			clean[index] = redactGeneric(nested)
		}
		return clean
	case string:
		return RedactMessage(typed)
	default:
		return value
	}
}

func sensitiveKey(key string) bool {
	compact := strings.ToLower(key)
	compact = strings.NewReplacer("_", "", "-", "", " ", "").Replace(compact)
	if compact == "code" || compact == "state" || compact == "codeverifier" || compact == "errordescription" {
		return true
	}
	if compact == "body" || compact == "error" || compact == "err" || compact == "cause" || strings.HasPrefix(compact, "rawerror") || strings.Contains(compact, "responsebody") || strings.Contains(compact, "responsepayload") {
		return true
	}
	for _, marker := range []string{"authorization", "cookie", "token", "secret", "signature", "credential", "accesskey"} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func redactURLString(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	if parsed.User != nil {
		return "[REDACTED URL]"
	}
	for key := range parsed.Query() {
		if sensitiveKey(key) {
			return "[REDACTED URL]"
		}
	}
	return value
}

var (
	urlPattern              = regexp.MustCompile(`https?://[^\s]+`)
	absoluteURLPattern      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s]+`)
	credentialHeaderPattern = regexp.MustCompile(`(?im)\b(?:authorization|cookie)\s*[:=][^\r\n]*(?:\r?\n[ \t]+[^\r\n]*)*`)
	bearerPattern           = regexp.MustCompile(`(?i)\bbearer\s+[^\s]+`)
	secretAssignmentPattern = regexp.MustCompile(`(?i)(cookie|[a-z0-9_-]*(?:token|secret|signature|credential|access[_-]?key)[a-z0-9_-]*)\s*[:=]\s*[^\s&]+`)
	responseBodyPattern     = regexp.MustCompile(`(?is)response\s+body\b.*`)
)

func redactAllURLs(message string) string {
	return absoluteURLPattern.ReplaceAllStringFunc(message, func(candidate string) string {
		trimmed := strings.TrimRight(candidate, ".,;)]}\"'")
		return "[REDACTED URL]" + candidate[len(trimmed):]
	})
}

// RedactMessage defensively removes secrets and signed URLs from diagnostics.
func RedactMessage(message string) string {
	message = responseBodyPattern.ReplaceAllString(message, "response body [REDACTED]")
	message = urlPattern.ReplaceAllStringFunc(message, func(candidate string) string {
		trimmed := strings.TrimRight(candidate, ".,;)")
		if redactURLString(trimmed) != trimmed {
			return "[REDACTED URL]" + candidate[len(trimmed):]
		}
		return candidate
	})
	message = credentialHeaderPattern.ReplaceAllStringFunc(message, func(header string) string {
		separator := strings.IndexAny(header, ":=")
		if separator < 0 {
			return "[REDACTED HEADER]"
		}
		return header[:separator+1] + " [REDACTED]"
	})
	message = bearerPattern.ReplaceAllString(message, "Bearer [REDACTED]")
	message = secretAssignmentPattern.ReplaceAllStringFunc(message, func(match string) string {
		separator := strings.IndexAny(match, ":=")
		if separator < 0 {
			return "[REDACTED]"
		}
		return match[:separator+1] + "[REDACTED]"
	})
	return message
}
