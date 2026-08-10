package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

// ErrOutcomeUnknown marks a non-idempotent request whose response does not
// prove whether the server committed the operation.
var ErrOutcomeUnknown = errors.New("non-idempotent operation outcome unknown")

// IsOutcomeUnknown reports whether replaying the failed operation could
// duplicate a remotely committed write.
func IsOutcomeUnknown(err error) bool { return errors.Is(err, ErrOutcomeUnknown) }

type outcomeUnknownError struct{ err error }

func (e *outcomeUnknownError) Error() string {
	if e == nil || e.err == nil {
		return ErrOutcomeUnknown.Error()
	}
	return e.err.Error()
}

func (e *outcomeUnknownError) Unwrap() []error {
	if e == nil || e.err == nil {
		return []error{ErrOutcomeUnknown}
	}
	return []error{ErrOutcomeUnknown, e.err}
}

func markOutcomeUnknown(err error) error {
	if err == nil || IsOutcomeUnknown(err) {
		return err
	}
	return &outcomeUnknownError{err: err}
}

func classifyNonIdempotentOutcome(err error) error {
	if err == nil {
		return nil
	}
	if IsRequestNotSent(err) {
		return err
	}
	var statusErr *responseStatusError
	if errors.As(err, &statusErr) {
		if statusErr.StatusCode >= 500 || statusErr.StatusCode == http.StatusRequestTimeout {
			return markOutcomeUnknown(err)
		}
		return err
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr != nil {
		switch appErr.Category {
		case apperr.Network, apperr.Interrupted:
			return markOutcomeUnknown(err)
		case apperr.Remote:
			// A successful HTTP response that cannot be decoded is outcome-unknown.
			return markOutcomeUnknown(err)
		}
	}
	return err
}

// StorageOptions describes the object-store multipart limits returned by AnyShare.
type StorageOptions struct {
	PartMinSize int64 `json:"partminsize"`
	PartMaxSize int64 `json:"partmaxsize"`
	PartMaxNum  int   `json:"partmaxnum"`
}

// SignedRequest is an object-store request authorized by AnyShare.
type SignedRequest struct {
	Method  string      `json:"-"`
	URL     string      `json:"-"`
	Headers http.Header `json:"-"`
}

// Checksums contains the hashes required by the upload protocol.
type Checksums struct {
	MD5      string
	SliceMD5 string
	CRC32    string
}

// DirectUploadRequest describes a new-file instant upload.
type DirectUploadRequest struct {
	ParentID, Name, ExistingID, EditedRev string
	Size, ClientMtimeUS                   int64
	Checksums                             Checksums
}

// BeginRequest describes a new upload or an overwrite upload.
type BeginRequest struct {
	ParentID, Name, ExistingID, EditedRev string
	Size, ClientMtimeUS                   int64
}

// FinishRequest finalizes an upload. EditedRev is the original target revision
// for an overwrite, not the newly allocated upload revision.
type FinishRequest struct {
	DocID, Rev, EditedRev string
	Checksums             Checksums
}

// BeginUpload identifies an initialized upload.
type BeginUpload struct {
	DocID, Rev, Name, UploadID string
	Request                    *SignedRequest
}

// PartAuthorization maps a multipart part number to its signed PUT request.
type PartAuthorization map[int]SignedRequest

// PartInfo is the completed object-store identity for one multipart part.
type PartInfo struct {
	ETag string
	Size int64
}

// UploadResult identifies a completed AnyShare document revision.
type UploadResult struct {
	DocID, Rev, Name string
	Modified         int64
}

// RefreshResult contains either a replacement single PUT or a multipart upload ID.
type RefreshResult struct {
	UploadID string
	Request  *SignedRequest
}

// StorageOptions returns AnyShare's object-store multipart limits.
func (c *Client) StorageOptions(ctx context.Context) (StorageOptions, error) {
	var options StorageOptions
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/osoption", nil, true, &options); err != nil {
		return StorageOptions{}, err
	}
	return options, nil
}

// PreUpload asks whether the server already has content matching the prefix hash.
func (c *Client) PreUpload(ctx context.Context, size int64, sliceMD5 string) (bool, error) {
	body, err := marshalBody(struct {
		Length   int64  `json:"length"`
		SliceMD5 string `json:"slice_md5"`
	}{Length: size, SliceMD5: sliceMD5})
	if err != nil {
		return false, err
	}
	var response struct {
		Match bool `json:"match"`
	}
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/predupload", body, true, &response); err != nil {
		return false, err
	}
	return response.Match, nil
}

// DirectUpload creates a new file from content already present in object storage.
func (c *Client) DirectUpload(ctx context.Context, req DirectUploadRequest) (UploadResult, error) {
	if req.ExistingID != "" || req.EditedRev != "" || req.ParentID == "" || req.Name == "" {
		return UploadResult{}, localUploadError("invalid direct upload request")
	}
	body, err := marshalBody(struct {
		CRC32       string `json:"crc32"`
		DocID       string `json:"docid"`
		Length      int64  `json:"length"`
		MD5         string `json:"md5"`
		ClientMtime int64  `json:"client_mtime"`
		Name        string `json:"name"`
		OnDup       int    `json:"ondup"`
	}{
		CRC32: strings.ToUpper(req.Checksums.CRC32), DocID: req.ParentID, Length: req.Size,
		MD5: strings.ToUpper(req.Checksums.MD5), ClientMtime: req.ClientMtimeUS, Name: req.Name, OnDup: 1,
	})
	if err != nil {
		return UploadResult{}, err
	}
	var result UploadResult
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/dupload", body, false, &result); err != nil {
		return UploadResult{}, classifyNonIdempotentOutcome(err)
	}
	if result.DocID == "" || result.Rev == "" {
		return UploadResult{}, markOutcomeUnknown(wrapError(apperr.Remote, "anyshare", "invalid direct upload response", errors.New("missing result identity")))
	}
	return result, nil
}

// BeginSingle initializes a single-object upload.
func (c *Client) BeginSingle(ctx context.Context, req BeginRequest) (BeginUpload, error) {
	return c.beginUpload(ctx, "/api/efast/v1/file/osbeginupload", req, true)
}

// InitMultipart initializes a multipart object upload.
func (c *Client) InitMultipart(ctx context.Context, req BeginRequest) (BeginUpload, error) {
	return c.beginUpload(ctx, "/api/efast/v1/file/osinitmultiupload", req, false)
}

func (c *Client) beginUpload(ctx context.Context, endpoint string, req BeginRequest, single bool) (BeginUpload, error) {
	overwrite := req.ExistingID != "" || req.EditedRev != ""
	if req.Size < 0 || (!overwrite && (req.ParentID == "" || req.Name == "")) || (overwrite && (req.ExistingID == "" || req.EditedRev == "" || req.ParentID != "" || req.Name != "")) {
		return BeginUpload{}, localUploadError("invalid begin upload request")
	}
	docID, name, onDup := req.ParentID, req.Name, 1
	if overwrite {
		docID, name, onDup = req.ExistingID, "", 0
	}
	reqMethod := ""
	if single {
		reqMethod = http.MethodPut
	}
	body, err := marshalBody(struct {
		DocID       string `json:"docid"`
		Length      int64  `json:"length"`
		Name        string `json:"name,omitempty"`
		ClientMtime int64  `json:"client_mtime"`
		OnDup       int    `json:"ondup,omitempty"`
		ReqMethod   string `json:"reqmethod,omitempty"`
		EditedRev   string `json:"editedrev,omitempty"`
	}{DocID: docID, Length: req.Size, Name: name, ClientMtime: req.ClientMtimeUS, OnDup: onDup, ReqMethod: reqMethod, EditedRev: req.EditedRev})
	if err != nil {
		return BeginUpload{}, err
	}
	var response struct {
		DocID       string          `json:"docid"`
		Rev         string          `json:"rev"`
		Name        string          `json:"name"`
		UploadID    string          `json:"uploadid"`
		AuthRequest json.RawMessage `json:"authrequest"`
	}
	if err := c.callJSON(ctx, http.MethodPost, endpoint, body, false, &response); err != nil {
		return BeginUpload{}, classifyNonIdempotentOutcome(err)
	}
	var request *SignedRequest
	if len(response.AuthRequest) != 0 && string(response.AuthRequest) != "null" {
		parsed, err := parseSignedRequest(response.AuthRequest)
		if err != nil {
			return BeginUpload{}, markOutcomeUnknown(wrapError(apperr.Remote, "anyshare", "invalid signed request", errors.New("signed request rejected")))
		}
		request = &parsed
	}
	if response.DocID == "" || response.Rev == "" || (single && (request == nil || response.UploadID != "")) || (!single && (response.UploadID == "" || request != nil)) {
		return BeginUpload{}, markOutcomeUnknown(wrapError(apperr.Remote, "anyshare", "invalid upload initialization response", errors.New("unexpected response variant")))
	}
	if overwrite && (response.DocID != req.ExistingID || response.Rev == req.EditedRev) {
		return BeginUpload{}, markOutcomeUnknown(wrapError(apperr.Remote, "anyshare", "invalid upload initialization response", errors.New("overwrite identity mismatch")))
	}
	return BeginUpload{DocID: response.DocID, Rev: response.Rev, Name: response.Name, UploadID: response.UploadID, Request: request}, nil
}

// AuthorizeParts obtains signed object-store requests for an inclusive part range.
func (c *Client) AuthorizeParts(ctx context.Context, docID, rev, uploadID string, first, last int) (PartAuthorization, error) {
	if first < 1 || last > 10000 || first > last {
		return nil, localUploadError("invalid multipart part range")
	}
	body, err := marshalBody(struct {
		DocID    string `json:"docid"`
		Rev      string `json:"rev"`
		UploadID string `json:"uploadid"`
		Parts    string `json:"parts"`
	}{DocID: docID, Rev: rev, UploadID: uploadID, Parts: strconv.Itoa(first) + "-" + strconv.Itoa(last)})
	if err != nil {
		return nil, err
	}
	var response partAuthorizationWire
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/osuploadpart", body, true, &response); err != nil {
		return nil, err
	}
	authorization := make(PartAuthorization, len(response.entries))
	for _, entry := range response.entries {
		rawPart, rawRequest := entry.part, entry.request
		part, err := strconv.Atoi(rawPart)
		if err != nil || strconv.Itoa(part) != rawPart || part < first || part > last || part < 1 || part > 10000 {
			return nil, wrapError(apperr.Remote, "anyshare", "invalid part authorization response", errors.New("invalid part number"))
		}
		if _, exists := authorization[part]; exists {
			return nil, wrapError(apperr.Remote, "anyshare", "invalid part authorization response", errors.New("duplicate part number"))
		}
		parsed, err := parseSignedRequest(rawRequest)
		if err != nil {
			return nil, wrapError(apperr.Remote, "anyshare", "invalid part authorization response", errors.New("signed request rejected"))
		}
		authorization[part] = parsed
	}
	if len(authorization) != last-first+1 {
		return nil, wrapError(apperr.Remote, "anyshare", "invalid part authorization response", errors.New("missing part authorization"))
	}
	return authorization, nil
}

type partAuthorizationEntry struct {
	part    string
	request json.RawMessage
}

type partAuthorizationWire struct {
	entries []partAuthorizationEntry
}

var deployedCompletionDelimiter = regexp.MustCompile(`[ \t\r\n]*--([A-Za-z0-9_]+)[ \t\r\n]*`)

func (wire *partAuthorizationWire) UnmarshalJSON(data []byte) error {
	entries, err := decodePartAuthorizationObject(data, true)
	if err != nil {
		return err
	}
	wire.entries = entries
	return nil
}

func decodePartAuthorizationObject(data []byte, allowEnvelope bool) ([]partAuthorizationEntry, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid part authorization object")
	}
	seen := make(map[string]struct{})
	entries := make([]partAuthorizationEntry, 0)
	var envelope json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid part authorization key")
		}
		part, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid part authorization key")
		}
		if _, exists := seen[part]; exists {
			return nil, errors.New("duplicate part authorization key")
		}
		seen[part] = struct{}{}
		var request json.RawMessage
		if err := decoder.Decode(&request); err != nil {
			return nil, errors.New("invalid part authorization request")
		}
		if part == "authrequests" {
			if !allowEnvelope || envelope != nil || len(entries) != 0 {
				return nil, errors.New("invalid part authorization envelope")
			}
			envelope = request
			continue
		}
		if envelope != nil {
			return nil, errors.New("invalid part authorization envelope")
		}
		entries = append(entries, partAuthorizationEntry{part: part, request: request})
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, errors.New("invalid part authorization object")
	}
	if decoder.More() {
		return nil, errors.New("invalid part authorization object")
	}
	if envelope != nil {
		return decodePartAuthorizationObject(envelope, false)
	}
	return entries, nil
}

// CompleteMultipart prepares the byte-exact object-store completion PUT.
func (c *Client) CompleteMultipart(ctx context.Context, docID, rev, uploadID string, parts map[int]PartInfo) (SignedRequest, []byte, error) {
	body, err := marshalCompleteMultipartBody(docID, rev, uploadID, parts)
	if err != nil {
		return SignedRequest{}, nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/efast/v1/file/oscompleteupload", body, false)
	if err != nil {
		return SignedRequest{}, nil, err
	}
	return parseCompleteMultipartResponse(resp)
}

func marshalCompleteMultipartBody(docID, rev, uploadID string, parts map[int]PartInfo) ([]byte, error) {
	partNumbers := make([]int, 0, len(parts))
	for part, info := range parts {
		if part < 1 || part > 10000 || info.Size < 0 {
			return nil, localUploadError("invalid multipart completion")
		}
		partNumbers = append(partNumbers, part)
	}
	sort.Ints(partNumbers)
	prefix, err := json.Marshal(struct {
		DocID    string `json:"docid"`
		Rev      string `json:"rev"`
		UploadID string `json:"uploadid"`
	}{DocID: docID, Rev: rev, UploadID: uploadID})
	if err != nil {
		return nil, wrapError(apperr.Local, "anyshare", "encode request", errors.New("invalid multipart completion"))
	}
	var body bytes.Buffer
	body.Write(prefix[:len(prefix)-1])
	body.WriteString(`,"partinfo":{`)
	for index, part := range partNumbers {
		if index != 0 {
			body.WriteByte(',')
		}
		key, _ := json.Marshal(strconv.Itoa(part))
		value, _ := json.Marshal([2]any{parts[part].ETag, parts[part].Size})
		body.Write(key)
		body.WriteByte(':')
		body.Write(value)
	}
	body.WriteString("}}")
	return body.Bytes(), nil
}

func parseCompleteMultipartResponse(resp *http.Response) (SignedRequest, []byte, error) {
	if resp == nil || resp.Body == nil {
		return SignedRequest{}, nil, invalidMultipartResponse()
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || int64(len(data)) > maxResponseBytes {
		return SignedRequest{}, nil, invalidMultipartResponse()
	}
	mediaType, params, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	boundary := ""
	if mediaErr == nil {
		boundary = params["boundary"]
	}
	if mediaErr == nil && strings.EqualFold(mediaType, "multipart/form-data") && boundary != "" {
		if signed, completionXML, ok := parseMIMECompletion(data, boundary); ok {
			return signed, completionXML, nil
		}
	}
	if signed, completionXML, ok := parseDeployedRawCompletion(data, boundary); ok {
		return signed, completionXML, nil
	}
	return SignedRequest{}, nil, invalidMultipartResponse()
}

func parseMIMECompletion(data []byte, boundary string) (SignedRequest, []byte, bool) {
	reader := multipart.NewReader(bytes.NewReader(data), boundary)
	var completionXML []byte
	var signed *SignedRequest
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SignedRequest{}, nil, false
		}
		partBody, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			return SignedRequest{}, nil, false
		}
		partMediaType, _, mediaErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if mediaErr != nil || len(bytes.TrimSpace(partBody)) == 0 {
			return SignedRequest{}, nil, false
		}
		switch {
		case strings.EqualFold(partMediaType, "application/json"):
			parsed, err := parseMultipartAuthorization(partBody)
			if err != nil || signed != nil {
				return SignedRequest{}, nil, false
			}
			signed = &parsed
		case strings.EqualFold(partMediaType, "application/xml"), strings.EqualFold(partMediaType, "text/xml"):
			if completionXML != nil {
				return SignedRequest{}, nil, false
			}
			completionXML = append([]byte(nil), partBody...)
		default:
			return SignedRequest{}, nil, false
		}
	}
	if signed == nil || completionXML == nil {
		return SignedRequest{}, nil, false
	}
	return *signed, completionXML, true
}

func parseDeployedRawCompletion(data []byte, _ string) (SignedRequest, []byte, bool) {
	matches := deployedCompletionDelimiter.FindAllSubmatchIndex(data, -1)
	if len(matches) != 3 || len(matches[0]) != 4 || len(matches[1]) != 4 || len(matches[2]) != 4 {
		return SignedRequest{}, nil, false
	}
	boundary := data[matches[0][2]:matches[0][3]]
	if !validRawBoundary(string(boundary)) || !bytes.Equal(boundary, data[matches[1][2]:matches[1][3]]) || !bytes.Equal(boundary, data[matches[2][2]:matches[2][3]]) {
		return SignedRequest{}, nil, false
	}
	if !onlyCompletionDelimiterPadding(data[:matches[0][0]]) || !onlyCompletionDelimiterPadding(data[matches[2][1]:]) {
		return SignedRequest{}, nil, false
	}
	completionXML := bytes.TrimSpace(data[matches[0][1]:matches[1][0]])
	authorizationJSON := bytes.TrimSpace(data[matches[1][1]:matches[2][0]])
	if !validCompletionXML(completionXML) || len(authorizationJSON) == 0 || authorizationJSON[0] != '{' {
		return SignedRequest{}, nil, false
	}
	signed, err := parseMultipartAuthorization(authorizationJSON)
	if err != nil {
		return SignedRequest{}, nil, false
	}
	return signed, append([]byte(nil), completionXML...), true
}

func onlyCompletionDelimiterPadding(data []byte) bool {
	for _, value := range data {
		if value != '-' && value != ' ' && value != '\t' && value != '\r' && value != '\n' {
			return false
		}
	}
	return true
}

func validRawBoundary(boundary string) bool {
	if len(boundary) < 1 || len(boundary) > 70 {
		return false
	}
	for index := 0; index < len(boundary); index++ {
		value := boundary[index]
		if value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value == '_' || value == '-' {
			continue
		}
		return false
	}
	return true
}

func validCompletionXML(body []byte) bool {
	var document struct {
		XMLName xml.Name
	}
	return len(body) != 0 && body[0] == '<' && xml.Unmarshal(body, &document) == nil && document.XMLName.Local == "CompleteMultipartUpload"
}

func parseMultipartAuthorization(body []byte) (SignedRequest, error) {
	var wrapped struct {
		AuthRequest json.RawMessage `json:"authrequest"`
	}
	if json.Unmarshal(body, &wrapped) == nil && len(wrapped.AuthRequest) != 0 {
		return parseCompletionSignedRequest(wrapped.AuthRequest)
	}
	return parseCompletionSignedRequest(json.RawMessage(body))
}

func invalidMultipartResponse() error {
	return wrapError(apperr.Remote, "anyshare", "invalid multipart completion response", errors.New("malformed response"))
}

// RefreshUpload refreshes authorization for an in-progress upload.
func (c *Client) RefreshUpload(ctx context.Context, docID, rev string, length int64, multipart bool) (RefreshResult, error) {
	reqMethod := ""
	if !multipart {
		reqMethod = http.MethodPut
	}
	body, err := marshalBody(struct {
		DocID     string `json:"docid"`
		Rev       string `json:"rev"`
		Length    int64  `json:"length"`
		Multi     bool   `json:"multiupload"`
		ReqMethod string `json:"reqmethod,omitempty"`
	}{DocID: docID, Rev: rev, Length: length, Multi: multipart, ReqMethod: reqMethod})
	if err != nil {
		return RefreshResult{}, err
	}
	var response struct {
		UploadID    string          `json:"uploadid"`
		AuthRequest json.RawMessage `json:"authrequest"`
	}
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/osuploadrefresh", body, false, &response); err != nil {
		return RefreshResult{}, err
	}
	var request *SignedRequest
	if len(response.AuthRequest) != 0 && string(response.AuthRequest) != "null" {
		parsed, err := parseSignedRequest(response.AuthRequest)
		if err != nil {
			return RefreshResult{}, wrapError(apperr.Remote, "anyshare", "invalid signed request", errors.New("signed request rejected"))
		}
		request = &parsed
	}
	if (multipart && (response.UploadID == "" || request != nil)) || (!multipart && (request == nil || response.UploadID != "")) {
		return RefreshResult{}, wrapError(apperr.Remote, "anyshare", "invalid upload refresh response", errors.New("unexpected response variant"))
	}
	return RefreshResult{UploadID: response.UploadID, Request: request}, nil
}

// FinishUpload commits an uploaded object as an AnyShare document revision.
func (c *Client) FinishUpload(ctx context.Context, req FinishRequest) (UploadResult, error) {
	body, err := marshalBody(struct {
		DocID     string `json:"docid"`
		Rev       string `json:"rev"`
		CRC32     string `json:"crc32"`
		MD5       string `json:"md5"`
		SliceMD5  string `json:"slice_md5"`
		EditedRev string `json:"editedrev,omitempty"`
	}{
		DocID: req.DocID, Rev: req.Rev, CRC32: strings.ToUpper(req.Checksums.CRC32),
		MD5: strings.ToUpper(req.Checksums.MD5), SliceMD5: strings.ToUpper(req.Checksums.SliceMD5), EditedRev: req.EditedRev,
	})
	if err != nil {
		return UploadResult{}, err
	}
	result := UploadResult{DocID: req.DocID, Rev: req.Rev}
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/osendupload", body, false, &result); err != nil {
		return UploadResult{}, classifyNonIdempotentOutcome(err)
	}
	if result.DocID == "" {
		result.DocID = req.DocID
	}
	if result.Rev == "" {
		result.Rev = req.Rev
	}
	return result, nil
}

func localUploadError(message string) error {
	return wrapError(apperr.Local, "anyshare", message, errors.New("invalid upload request"))
}
