package anyshare

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

const maxSignedErrorBytes int64 = 64 << 10

// ErrExpiredSignature marks an object-store authorization that can be refreshed.
var ErrExpiredSignature = errors.New("object-store signature expired")

// SignedError describes a rejected object-store request without retaining its
// URL, authorization headers, or raw response body.
type SignedError struct {
	StatusCode int
	Code       string
	expired    bool
}

func (e *SignedError) Error() string {
	if e == nil {
		return ""
	}
	return "object-store request failed (status " + strconv.Itoa(e.StatusCode) + ")"
}

// Unwrap exposes only the refreshable-expiration sentinel.
func (e *SignedError) Unwrap() error {
	if e != nil && e.expired {
		return ErrExpiredSignature
	}
	return nil
}

// IsExpiredSignature reports whether err is an explicitly recognized signature expiry.
func IsExpiredSignature(err error) bool {
	return errors.Is(err, ErrExpiredSignature)
}

// String prevents signed request material from appearing in ordinary diagnostics.
func (SignedRequest) String() string { return "signed request [REDACTED]" }

// GoString prevents signed request material from appearing in Go-syntax diagnostics.
func (SignedRequest) GoString() string { return "anyshare.SignedRequest{[REDACTED]}" }

func parseSignedRequest(raw json.RawMessage) (SignedRequest, error) {
	signed, err := parseSignedRequestAnyMethod(raw)
	if err != nil || signed.Method != http.MethodPut {
		return SignedRequest{}, errors.New("invalid signed request")
	}
	return signed, nil
}

func parseCompletionSignedRequest(raw json.RawMessage) (SignedRequest, error) {
	signed, err := parseSignedRequestAnyMethod(raw)
	if err != nil || (signed.Method != http.MethodPost && signed.Method != http.MethodPut) {
		return SignedRequest{}, errors.New("invalid signed request")
	}
	signed.Headers.Del("x-as-userid")
	return signed, nil
}

func parseDownloadSignedRequest(raw json.RawMessage) (SignedRequest, error) {
	signed, err := parseSignedRequestAnyMethod(raw)
	if err != nil || signed.Method != http.MethodGet {
		return SignedRequest{}, errors.New("invalid signed request")
	}
	// The deployed QUERY_STRING download flow uses only the method and URL;
	// renderer clients intentionally do not forward control-plane headers to
	// the object host.
	signed.Headers = make(http.Header)
	return signed, nil
}

func parseSignedRequestAnyMethod(raw json.RawMessage) (SignedRequest, error) {
	var fields []string
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) < 2 {
		return SignedRequest{}, errors.New("invalid signed request")
	}
	headers := make(http.Header)
	for _, line := range fields[2:] {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return SignedRequest{}, errors.New("invalid signed request")
		}
		name := parts[0]
		if name != strings.TrimSpace(name) {
			return SignedRequest{}, errors.New("invalid signed request")
		}
		value := strings.TrimSpace(parts[1])
		if !validHeaderName(name) || !validHeaderValue(value) {
			return SignedRequest{}, errors.New("invalid signed request")
		}
		headers.Add(name, value)
	}
	return validateSignedRequest(SignedRequest{Method: fields[0], URL: fields[1], Headers: headers})
}

func validateSignedRequest(signed SignedRequest) (SignedRequest, error) {
	if signed.Method != http.MethodPut && signed.Method != http.MethodPost && signed.Method != http.MethodGet {
		return SignedRequest{}, errors.New("invalid signed request")
	}
	parsed, err := url.Parse(signed.URL)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return SignedRequest{}, errors.New("invalid signed request")
	}
	headers := make(http.Header, len(signed.Headers))
	for name, values := range signed.Headers {
		if !validHeaderName(name) || len(values) == 0 {
			return SignedRequest{}, errors.New("invalid signed request")
		}
		for _, value := range values {
			if !validHeaderValue(value) || (strings.EqualFold(name, "Authorization") && bearerAuthorization(value)) {
				return SignedRequest{}, errors.New("invalid signed request")
			}
			headers.Add(name, value)
		}
	}
	return SignedRequest{Method: signed.Method, URL: parsed.String(), Headers: headers}, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !tokenByte(name[i]) {
			return false
		}
	}
	return true
}

func tokenByte(value byte) bool {
	if value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] != '\t' && (value[i] < 0x20 || value[i] == 0x7f) {
			return false
		}
	}
	return true
}

func bearerAuthorization(value string) bool {
	fields := strings.Fields(value)
	return len(fields) > 0 && strings.EqualFold(fields[0], "Bearer")
}

// PutSigned performs an isolated object-store request using only server-issued headers.
func (c *Client) PutSigned(ctx context.Context, signed SignedRequest, body io.Reader, getBody func() (io.ReadCloser, error), length int64) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.httpClient == nil || c.httpClient.HTTP == nil || length < 0 {
		return "", wrapError(apperr.Local, "anyshare", "invalid signed request", errors.New("object-store transport is not configured"))
	}
	validated, err := validateSignedRequest(signed)
	if err != nil || (validated.Method != http.MethodPut && validated.Method != http.MethodPost) {
		return "", wrapError(apperr.Local, "anyshare", "invalid signed request", errors.New("signed request rejected"))
	}
	req, err := http.NewRequestWithContext(ctx, validated.Method, validated.URL, body)
	if err != nil {
		return "", wrapError(apperr.Local, "anyshare", "invalid signed request", errors.New("signed request rejected"))
	}
	req.Header = validated.Headers.Clone()
	req.Header.Del("Content-Length")
	if hostValues, exists := req.Header["Host"]; exists {
		if len(hostValues) != 1 || hostValues[0] == "" || !validHeaderValue(hostValues[0]) {
			return "", wrapError(apperr.Local, "anyshare", "invalid signed request", errors.New("signed request rejected"))
		}
		req.Host = hostValues[0]
		req.Header.Del("Host")
	}
	if _, signedUserAgent := req.Header["User-Agent"]; !signedUserAgent {
		req.Header["User-Agent"] = nil
	}
	req.ContentLength = length
	req.GetBody = getBody

	isolated := *c.httpClient
	standardClient := *c.httpClient.HTTP
	if transport, ok := standardClient.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.DisableCompression = true
		standardClient.Transport = transport
	}
	if err := configurePinnedObjectTransport(&standardClient, req.URL.Hostname(), c.server); err != nil {
		return "", wrapError(apperr.Local, "anyshare", "invalid object-store trust configuration", errors.New("object-store trust configuration rejected"))
	}
	standardClient.CheckRedirect = signedRedirectPolicy
	isolated.HTTP = &standardClient
	resp, err := isolated.Do(ctx, req, validated.Method == http.MethodPut)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", wrapError(apperr.Interrupted, "anyshare", "object-store request interrupted", contextErr)
		}
		category := apperr.Network
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			category = appErr.Category
		}
		return "", wrapError(category, "anyshare", "object-store request failed", errors.New("transport error"))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", signedResponseError(resp)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		etag = resp.Header.Get("Content-MD5")
	}
	discardSignedBody(resp.Body)
	return etag, nil
}

func configurePinnedObjectTransport(client *http.Client, hostname, controlServer string) error {
	rawPin, err := configuredObjectSPKIPin(controlServer)
	if err != nil {
		return err
	}
	if rawPin == "" {
		return nil
	}
	if !validObjectSPKIPin(rawPin) {
		return errors.New("invalid object-store SPKI pin")
	}
	expectedPin, err := hex.DecodeString(rawPin)
	if err != nil || len(expectedPin) != sha256.Size || hostname == "" {
		return errors.New("invalid object-store SPKI pin")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return errors.New("object-store transport does not support pinning")
	}
	transport = transport.Clone()
	transport.Proxy = nil
	tlsConfig := transport.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	previousVerifyConnection := tlsConfig.VerifyConnection
	// The deployed AnyShare object endpoint presents a hostname-valid leaf
	// without a publicly trusted chain. In this explicit compatibility mode,
	// normal CA verification is replaced only for the isolated signed object
	// transport by an exact SPKI pin plus hostname and validity checks.
	tlsConfig.InsecureSkipVerify = true //nolint:gosec
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("object-store certificate is missing")
		}
		leaf := state.PeerCertificates[0]
		now := time.Now()
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return errors.New("object-store certificate is outside its validity period")
		}
		if err := leaf.VerifyHostname(hostname); err != nil {
			return errors.New("object-store certificate hostname mismatch")
		}
		actualPin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(actualPin[:], expectedPin) != 1 {
			return errors.New("object-store certificate pin mismatch")
		}
		if previousVerifyConnection != nil {
			return previousVerifyConnection(state)
		}
		return nil
	}
	transport.TLSClientConfig = tlsConfig
	client.Transport = transport
	return nil
}

func signedResponseError(resp *http.Response) error {
	if resp == nil {
		return wrapError(apperr.Network, "anyshare", "object-store request failed", errors.New("missing response"))
	}
	body, oversized := readSignedErrorBody(resp.Body)
	code, message := parseSignedErrorBody(body)
	expired := !oversized && explicitExpiration(resp.StatusCode, code, message)
	signedErr := &SignedError{StatusCode: resp.StatusCode, Code: sanitizeSignedCode(code), expired: expired}
	return wrapError(apperr.Remote, "anyshare", "object-store request rejected", signedErr)
}

func readSignedErrorBody(body io.ReadCloser) ([]byte, bool) {
	if body == nil {
		return nil, false
	}
	defer body.Close()
	data, _ := io.ReadAll(io.LimitReader(body, maxSignedErrorBytes+1))
	if int64(len(data)) > maxSignedErrorBytes {
		return nil, true
	}
	return data, false
}

func signedRedirectPolicy(*http.Request, []*http.Request) error {
	return errors.New("signed redirect rejected")
}

func discardSignedBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxSignedErrorBytes+1))
	_ = body.Close()
}

func parseSignedErrorBody(body []byte) (code, message string) {
	var xmlError struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &xmlError) == nil && (xmlError.Code != "" || xmlError.Message != "") {
		return strings.TrimSpace(xmlError.Code), strings.TrimSpace(xmlError.Message)
	}
	var jsonError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &jsonError) == nil {
		message = jsonError.Message
		if message == "" {
			message = jsonError.Error
		}
		return strings.TrimSpace(jsonError.Code), strings.TrimSpace(message)
	}
	return "", strings.TrimSpace(string(body))
}

func explicitExpiration(status int, code, message string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "requesttimetooskewed", "expiredtoken", "securitytokenexpired":
		return true
	}
	lowerMessage := strings.ToLower(message)
	return strings.Contains(lowerMessage, "request has expired") ||
		strings.Contains(lowerMessage, "expired token") ||
		strings.Contains(lowerMessage, "token out of date") ||
		strings.Contains(lowerMessage, "request time too skewed")
}

func sanitizeSignedCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 128 {
		return ""
	}
	for i := 0; i < len(code); i++ {
		value := code[i]
		if !(value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || strings.ContainsRune("._-", rune(value))) {
			return ""
		}
	}
	return code
}
