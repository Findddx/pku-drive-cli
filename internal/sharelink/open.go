package sharelink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
)

const maxLinkResponseBytes int64 = 1 << 20

var opaqueLinkID = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// LinkType identifies the authorization model of a share.
type LinkType string

const (
	LinkAnonymous LinkType = "anonymous"
	LinkRealname  LinkType = "realname"
)

// Opener creates a fresh, in-memory HTTP session for each share link.
type Opener struct {
	server        string
	origin        *url.URL
	httpClient    *httpx.Client
	authenticated *anyshare.Client
}

// NewOpener constructs a same-origin shared-link opener. authenticated may be
// nil when only anonymous links are needed.
func NewOpener(server string, httpClient *httpx.Client, authenticated *anyshare.Client) (*Opener, error) {
	parsed, normalized, err := normalizeServer(server)
	if err != nil || httpClient == nil || httpClient.HTTP == nil {
		return nil, apperr.Wrap(apperr.Local, "share link", "invalid client configuration", errors.New("invalid configuration"))
	}
	return &Opener{server: normalized, origin: parsed, httpClient: httpClient, authenticated: authenticated}, nil
}

type metadataWire struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Title            string          `json:"title"`
	ExpiresAt        json.RawMessage `json:"expires_at"`
	PasswordRequired *bool           `json:"password_required"`
	VerifyMobile     *bool           `json:"verify_mobile"`
	Item             struct {
		BelongsTo string `json:"belongs_to"`
		ID        string `json:"id"`
		Type      string `json:"type"`
		Name      string `json:"name"`
	} `json:"item"`
}

type landingInfo struct {
	linkType         string
	itemType         string
	title            string
	expiresAt        string
	passwordRequired *bool
	verifyMobile     *bool
}

// Open validates rawLink, resolves its public metadata without OAuth, and
// returns a session backed by either the ephemeral anonymous credential or the
// caller's authenticated AnyShare client.
func (o *Opener) Open(ctx context.Context, rawLink string) (*Session, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if o == nil || o.origin == nil || o.httpClient == nil || o.httpClient.HTTP == nil {
		return nil, apperr.Wrap(apperr.Local, "share link", "client is not configured", errors.New("missing client dependency"))
	}
	linkURL, linkID, err := o.parseLink(rawLink)
	if err != nil {
		return nil, err
	}
	metadata, err := o.fetchMetadata(ctx, o.metadataClient(), linkID)
	if err != nil {
		return nil, err
	}
	linkType, rootType, title, err := validateMetadata(metadata, landingInfo{}, linkID)
	if err != nil {
		return nil, err
	}
	if err := restrictions(metadata, landingInfo{}); err != nil {
		return nil, err
	}

	var api *anyshare.Client
	var root anyshare.Item
	switch linkType {
	case LinkAnonymous:
		privateClient, tokenCapture, err := o.privateClient(linkID)
		if err != nil {
			return nil, err
		}
		landing, err := o.fetchLanding(ctx, privateClient, linkURL)
		if err != nil {
			return nil, err
		}
		linkType, rootType, title, err = validateMetadata(metadata, landing, linkID)
		if err != nil {
			return nil, err
		}
		if err := restrictions(metadata, landing); err != nil {
			return nil, err
		}
		token := tokenCapture.Token()
		if token == "" {
			return nil, authRequiredError()
		}
		// The legacy cookie only bootstraps the short-lived bearer token. Keep
		// the browsing jar out of the API and signed-object transports.
		api = anyshare.NewClient(o.server, o.metadataClient(), memoryToken(token))
		items, err := api.EntryItems(ctx)
		if err != nil {
			return nil, classifyAPIError(err)
		}
		root, err = selectAnonymousRoot(items, metadata.Item.ID, rootType)
		if err != nil {
			return nil, err
		}
	case LinkRealname:
		if o.authenticated == nil {
			return nil, authRequiredError()
		}
		api = o.authenticated
		if rootType == "file" {
			root, err = api.FileMetadata(ctx, metadata.Item.ID, "")
			if err != nil {
				return nil, classifyAPIError(err)
			}
			if itemID(root) != metadata.Item.ID {
				return nil, invalidRemoteError("file metadata did not match shared item")
			}
		} else {
			root = anyshare.Item{ID: metadata.Item.ID, DocID: metadata.Item.ID, Name: title, Type: "directory", Size: -1}
		}
	default:
		return nil, invalidRemoteError("unsupported link type")
	}
	rootEntry, err := makeRootEntry(root)
	if err != nil {
		return nil, err
	}
	return newSession(linkType, api, rootEntry), nil
}

// metadataClient deliberately has neither a cookie jar nor redirect support.
// Resolving a link's public type must not depend on a browser session.
func (o *Opener) metadataClient() *httpx.Client {
	standard := *o.httpClient.HTTP
	standard.Jar = nil
	standard.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("metadata redirect rejected")
	}
	client := *o.httpClient
	client.HTTP = &standard
	return &client
}

func normalizeServer(raw string) (*url.URL, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || strings.HasSuffix(parsed.Host, ":") || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, "", errors.New("invalid server")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed, strings.TrimRight(parsed.String(), "/"), nil
}

func (o *Opener) parseLink(raw string) (*url.URL, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || strings.HasSuffix(parsed.Host, ":") || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, "", invalidLinkError()
	}
	if !sameOrigin(parsed, o.origin) || parsed.RawPath != "" || parsed.EscapedPath() != parsed.Path {
		return nil, "", invalidLinkError()
	}
	const prefix = "/link/"
	if !strings.HasPrefix(parsed.Path, prefix) {
		return nil, "", invalidLinkError()
	}
	id := strings.TrimPrefix(parsed.Path, prefix)
	if id == "" || len(id) > 256 || !opaqueLinkID.MatchString(id) {
		return nil, "", invalidLinkError()
	}
	return parsed, id, nil
}

func (o *Opener) privateClient(linkID string) (*httpx.Client, *linkTokenTransport, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.Local, "share link", "create private session", errors.New("cookie jar unavailable"))
	}
	standard := *o.httpClient.HTTP
	standard.Jar = jar
	baseTransport := standard.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	tokenCapture := &linkTokenTransport{
		base:       baseTransport,
		origin:     o.origin,
		cookieName: "link_token:" + linkID,
	}
	standard.Transport = tokenCapture
	standard.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !sameOrigin(req.URL, o.origin) {
			return errors.New("redirect rejected")
		}
		return nil
	}
	private := *o.httpClient
	private.HTTP = &standard
	return &private, tokenCapture, nil
}

func (o *Opener) fetchLanding(ctx context.Context, client *httpx.Client, linkURL *url.URL) (landingInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, linkURL.String(), nil)
	if err != nil {
		return landingInfo{}, invalidLinkError()
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(ctx, req, true)
	if err != nil {
		return landingInfo{}, requestError(ctx)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return landingInfo{}, statusError(resp.StatusCode)
	}
	if resp.Request == nil || resp.Request.URL == nil || !sameOrigin(resp.Request.URL, o.origin) {
		return landingInfo{}, invalidRemoteError("invalid landing response")
	}
	return parseLanding(resp.Request.URL.Query())
}

func (o *Opener) fetchMetadata(ctx context.Context, client *httpx.Client, linkID string) (metadataWire, error) {
	endpoint := o.server + "/api/shared-link/v1/links/" + url.PathEscape(linkID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return metadataWire{}, invalidRemoteError("could not build metadata request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(ctx, req, true)
	if err != nil {
		return metadataWire{}, requestError(ctx)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		drainAndClose(resp.Body)
		return metadataWire{}, statusError(resp.StatusCode)
	}
	var metadata metadataWire
	if err := httpx.DecodeJSON(resp, maxLinkResponseBytes, &metadata); err != nil {
		return metadataWire{}, invalidRemoteError("invalid metadata response")
	}
	return metadata, nil
}

func parseLanding(values url.Values) (landingInfo, error) {
	for _, key := range []string{"type", "item_type", "title", "expires_at", "password_required", "verify_mobile"} {
		if all, ok := values[key]; ok && len(all) != 1 {
			return landingInfo{}, invalidRemoteError("ambiguous landing metadata")
		}
	}
	result := landingInfo{
		linkType:  values.Get("type"),
		itemType:  values.Get("item_type"),
		title:     values.Get("title"),
		expiresAt: values.Get("expires_at"),
	}
	var err error
	if raw, ok, valueErr := singleQueryValue(values, "password_required"); valueErr != nil {
		err = invalidRemoteError("invalid landing restrictions")
	} else if ok {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			err = invalidRemoteError("invalid landing restrictions")
		} else {
			result.passwordRequired = &value
		}
	}
	if raw, ok, valueErr := singleQueryValue(values, "verify_mobile"); valueErr != nil {
		err = invalidRemoteError("invalid landing restrictions")
	} else if ok {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			err = invalidRemoteError("invalid landing restrictions")
		} else {
			result.verifyMobile = &value
		}
	}
	return result, err
}

func singleQueryValue(values url.Values, key string) (string, bool, error) {
	all, ok := values[key]
	if !ok {
		return "", false, nil
	}
	if len(all) != 1 || all[0] == "" {
		return "", false, errors.New("invalid query value")
	}
	return all[0], true, nil
}

func validateMetadata(metadata metadataWire, landing landingInfo, linkID string) (LinkType, string, string, error) {
	if metadata.ID != linkID || metadata.Item.BelongsTo != "document" || (metadata.Item.ID != "" && !strings.HasPrefix(metadata.Item.ID, "gns://")) {
		return "", "", "", invalidRemoteError("invalid shared item metadata")
	}
	linkType := LinkType(metadata.Type)
	if linkType != LinkAnonymous && linkType != LinkRealname {
		return "", "", "", invalidRemoteError("unsupported link type")
	}
	if linkType == LinkRealname && metadata.Item.ID == "" {
		return "", "", "", invalidRemoteError("real-name shared item identity is missing")
	}
	if landing.linkType != "" && landing.linkType != metadata.Type {
		return "", "", "", invalidRemoteError("inconsistent link type")
	}
	rootType, ok := normalizeItemType(metadata.Item.Type)
	if !ok {
		return "", "", "", invalidRemoteError("unsupported shared item type")
	}
	if landing.itemType != "" {
		landingType, valid := normalizeItemType(landing.itemType)
		if !valid || landingType != rootType {
			return "", "", "", invalidRemoteError("inconsistent shared item type")
		}
	}
	title := metadata.Title
	if title == "" {
		title = metadata.Item.Name
	}
	if title == "" {
		title = landing.title
	}
	return linkType, rootType, title, nil
}

func restrictions(metadata metadataWire, landing landingInfo) error {
	if metadata.PasswordRequired != nil && *metadata.PasswordRequired || landing.passwordRequired != nil && *landing.passwordRequired {
		return passwordRequiredError()
	}
	if metadata.VerifyMobile != nil && *metadata.VerifyMobile || landing.verifyMobile != nil && *landing.verifyMobile {
		return mobileVerificationRequiredError()
	}
	metadataExpiry, metadataPresent, err := parseExpiry(metadata.ExpiresAt, "")
	if err != nil {
		return invalidRemoteError("invalid link expiry")
	}
	landingExpiry, landingPresent, err := parseExpiry(nil, landing.expiresAt)
	if err != nil {
		return invalidRemoteError("invalid link expiry")
	}
	now := time.Now()
	// The documented Unix epoch value denotes a link with no expiry.
	if metadataPresent && metadataExpiry.After(time.Unix(0, 0)) && metadataExpiry.Before(now) || landingPresent && landingExpiry.After(time.Unix(0, 0)) && landingExpiry.Before(now) {
		return expiredError()
	}
	return nil
}

func parseExpiry(raw json.RawMessage, fallback string) (time.Time, bool, error) {
	value := ""
	if len(raw) > 0 && string(raw) != "null" {
		if raw[0] == '"' {
			if err := json.Unmarshal(raw, &value); err != nil {
				return time.Time{}, false, err
			}
		} else {
			value = string(raw)
		}
	} else {
		value = fallback
	}
	if value == "" {
		return time.Time{}, false, nil
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(seconds, 0), true, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false, err
	}
	return parsed, true, nil
}

func selectAnonymousRoot(items []anyshare.Item, expectedID, expectedType string) (anyshare.Item, error) {
	var root anyshare.Item
	matches := 0
	if expectedID == "" {
		if len(items) != 1 {
			return anyshare.Item{}, invalidRemoteError("anonymous entry root is ambiguous")
		}
		root = items[0]
		matches = 1
	} else {
		for _, item := range items {
			if itemID(item) == expectedID {
				root = item
				matches++
			}
		}
	}
	if matches == 0 {
		return anyshare.Item{}, notFoundError()
	}
	if matches != 1 {
		return anyshare.Item{}, invalidRemoteError("ambiguous anonymous entry")
	}
	typeName, ok := normalizeItemType(root.Type)
	if !ok || typeName != expectedType {
		return anyshare.Item{}, invalidRemoteError("anonymous entry did not match shared item")
	}
	root.Type = typeName
	if itemID(root) == "" {
		return anyshare.Item{}, invalidRemoteError("anonymous entry identity is missing")
	}
	return root, nil
}

func normalizeItemType(value string) (string, bool) {
	switch value {
	case "file":
		return "file", true
	case "folder", "directory":
		return "directory", true
	default:
		return "", false
	}
}

func itemID(item anyshare.Item) string {
	if item.ID != "" {
		return item.ID
	}
	return item.DocID
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && left.Scheme == "https" && right.Scheme == "https" &&
		strings.EqualFold(left.Hostname(), right.Hostname()) && effectiveHTTPSPort(left) == effectiveHTTPSPort(right)
}

func effectiveHTTPSPort(value *url.URL) string {
	if value == nil {
		return ""
	}
	if port := value.Port(); port != "" {
		return port
	}
	return "443"
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

type memoryToken string

func (t memoryToken) Token(context.Context, bool) (string, error) { return string(t), nil }
