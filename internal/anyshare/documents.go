package anyshare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

// Item is a normalized document, directory, or entry library.
type Item struct {
	ID            string `json:"id"`
	DocID         string `json:"docid"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	Type          string `json:"type"`
	Rev           string `json:"rev"`
	Size          int64  `json:"size"`
	Modified      int64  `json:"modified"`
	ClientMtimeUS int64  `json:"client_mtime"`
	MD5           string `json:"md5"`
	SliceMD5      string `json:"slice_md5"`
	CRC32         string `json:"crc32"`
}

// Page is one normalized page of folder entries.
type Page struct {
	Entries []Item
	Marker  string
}

// DeleteStatus describes whether a delete was applied immediately or accepted
// for an administrator-configured review workflow.
type DeleteStatus string

const (
	DeleteStatusDeleted       DeleteStatus = "deleted"
	DeleteStatusPendingReview DeleteStatus = "pending_review"
)

// DeleteResult is the outcome reported by an AnyShare delete endpoint.
type DeleteResult struct {
	Status DeleteStatus `json:"status"`
}

type itemWire struct {
	ID             string             `json:"id"`
	DocID          string             `json:"docid"`
	Name           string             `json:"name"`
	Path           string             `json:"path"`
	Type           string             `json:"type"`
	Rev            string             `json:"rev"`
	Size           int64              `json:"size"`
	ClientMtime    int64              `json:"client_mtime"`
	CustomMetadata customMetadataWire `json:"custom_metadata"`
	Modified       json.RawMessage    `json:"modified"`
	ModifiedAt     string             `json:"modified_at"`
	MD5            string             `json:"md5"`
	SliceMD5       string             `json:"slice_md5"`
	CRC32          string             `json:"crc32"`
}

type customMetadataWire struct {
	ClientMtime *int64 `json:"client_mtime"`
}

type folderPageWire struct {
	Dirs       []itemWire `json:"dirs"`
	Files      []itemWire `json:"files"`
	NextMarker string     `json:"next_marker"`
}

// EntryDocLibs returns the authenticated user's entry document libraries.
func (c *Client) EntryDocLibs(ctx context.Context) ([]Item, error) {
	var wireItems []itemWire
	if err := c.callJSON(ctx, http.MethodGet, "/api/efast/v1/entry-doc-lib?direction=asc&sort=doc_lib_name", nil, true, &wireItems); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(wireItems))
	for _, wireItem := range wireItems {
		item, err := normalizeItem(wireItem, wireItem.Type)
		if err != nil {
			return nil, err
		}
		item.Path = "/" + item.Name
		items = append(items, item)
	}
	return items, nil
}

// ListFolderPage returns one page of a folder's children.
func (c *Client) ListFolderPage(ctx context.Context, id, marker string) (Page, error) {
	endpoint := "/api/efast/v1/folders/" + url.PathEscape(id) + "/sub_objects?limit=1000"
	if marker != "" {
		endpoint += "&marker=" + url.QueryEscape(marker)
	}
	var wirePage folderPageWire
	if err := c.callJSON(ctx, http.MethodGet, endpoint, nil, true, &wirePage); err != nil {
		return Page{}, err
	}
	page := Page{Entries: make([]Item, 0, len(wirePage.Dirs)+len(wirePage.Files)), Marker: wirePage.NextMarker}
	for _, wireItem := range wirePage.Dirs {
		item, err := normalizeItem(wireItem, "directory")
		if err != nil {
			return Page{}, err
		}
		page.Entries = append(page.Entries, item)
	}
	for _, wireItem := range wirePage.Files {
		item, err := normalizeItem(wireItem, "file")
		if err != nil {
			return Page{}, err
		}
		page.Entries = append(page.Entries, item)
	}
	return page, nil
}

// ListFolder returns all pages of a folder's children.
func (c *Client) ListFolder(ctx context.Context, id string) ([]Item, error) {
	var entries []Item
	marker := ""
	seen := make(map[string]struct{})
	for {
		page, err := c.ListFolderPage(ctx, id, marker)
		if err != nil {
			return nil, err
		}
		entries = append(entries, page.Entries...)
		if page.Marker == "" {
			return entries, nil
		}
		if _, exists := seen[page.Marker]; exists {
			return nil, wrapError(apperr.Remote, "anyshare", "folder listing repeated a page marker", errors.New("invalid pagination response"))
		}
		seen[page.Marker] = struct{}{}
		marker = page.Marker
	}
}

// ResolveNamePath resolves an AnyShare display path to a normalized item.
func (c *Client) ResolveNamePath(ctx context.Context, namePath string) (Item, error) {
	body, err := marshalBody(struct {
		NamePath string `json:"namepath"`
	}{NamePath: namePath})
	if err != nil {
		return Item{}, err
	}
	var wireItem itemWire
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/getinfobypath", body, true, &wireItem); err != nil {
		return Item{}, err
	}
	return normalizeItem(wireItem, wireItem.Type)
}

// CreateDir creates a child directory.
func (c *Client) CreateDir(ctx context.Context, parentID, name string) (Item, error) {
	body, err := marshalBody(struct {
		DocID string `json:"docid"`
		Name  string `json:"name"`
		OnDup int    `json:"ondup"`
	}{DocID: parentID, Name: name, OnDup: 1})
	if err != nil {
		return Item{}, err
	}
	var wireItem itemWire
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/dir/create", body, false, &wireItem); err != nil {
		return Item{}, err
	}
	item, err := normalizeItem(wireItem, "directory")
	if err != nil {
		return Item{}, err
	}
	if item.Name == "" {
		item.Name = name
	}
	return item, nil
}

// FileMetadata returns metadata for an exact document ID and revision.
func (c *Client) FileMetadata(ctx context.Context, docID, rev string) (Item, error) {
	body, err := marshalBody(struct {
		DocID string `json:"docid"`
		Rev   string `json:"rev,omitempty"`
	}{DocID: docID, Rev: rev})
	if err != nil {
		return Item{}, err
	}
	var wireItem itemWire
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/metadata", body, true, &wireItem); err != nil {
		return Item{}, err
	}
	return normalizeItem(wireItem, "file")
}

// DeleteFile moves the file identified by the exact document ID to the
// document library's recycle bin, or reports that review is pending.
func (c *Client) DeleteFile(ctx context.Context, docID string) (DeleteResult, error) {
	body, err := marshalBody(struct {
		DocID string `json:"docid"`
	}{DocID: docID})
	if err != nil {
		return DeleteResult{}, err
	}
	return c.delete(ctx, "/api/efast/v1/file/delete", body)
}

// DeleteDir moves the directory tree identified by the exact document ID to
// the document library's recycle bin, or reports that review is pending. The
// upload-process guard is always enabled so an in-progress child upload is
// never bypassed silently.
func (c *Client) DeleteDir(ctx context.Context, docID string) (DeleteResult, error) {
	body, err := marshalBody(struct {
		DocID              string `json:"docid"`
		CheckUploadProcess bool   `json:"check_upload_process"`
	}{DocID: docID, CheckUploadProcess: true})
	if err != nil {
		return DeleteResult{}, err
	}
	return c.delete(ctx, "/api/efast/v1/dir/delete", body)
}

func (c *Client) delete(ctx context.Context, endpoint string, body []byte) (DeleteResult, error) {
	resp, err := c.do(ctx, http.MethodPost, endpoint, body, false)
	if err != nil {
		return DeleteResult{}, err
	}
	discardAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return DeleteResult{Status: DeleteStatusDeleted}, nil
	case http.StatusAccepted:
		return DeleteResult{Status: DeleteStatusPendingReview}, nil
	default:
		return DeleteResult{}, wrapError(apperr.Remote, "anyshare", "unexpected delete response", &responseStatusError{StatusCode: resp.StatusCode})
	}
}

func normalizeItem(wireItem itemWire, stableType string) (Item, error) {
	modified, err := normalizeModified(wireItem.Modified, wireItem.ModifiedAt)
	if err != nil {
		return Item{}, wrapError(apperr.Remote, "anyshare", "invalid item timestamp", err)
	}
	id := wireItem.ID
	if id == "" {
		id = wireItem.DocID
	}
	if stableType == "" {
		stableType = "file"
		if wireItem.Size == -1 {
			stableType = "directory"
		}
	}
	clientMtime := wireItem.ClientMtime
	// Official nested metadata takes precedence when present, including zero.
	if wireItem.CustomMetadata.ClientMtime != nil {
		clientMtime = *wireItem.CustomMetadata.ClientMtime
	}
	return Item{
		ID:            id,
		DocID:         wireItem.DocID,
		Name:          wireItem.Name,
		Path:          wireItem.Path,
		Type:          stableType,
		Rev:           wireItem.Rev,
		Size:          wireItem.Size,
		Modified:      modified,
		ClientMtimeUS: clientMtime,
		MD5:           wireItem.MD5,
		SliceMD5:      wireItem.SliceMD5,
		CRC32:         wireItem.CRC32,
	}, nil
}

func normalizeModified(legacy json.RawMessage, modifiedAt string) (int64, error) {
	if modifiedAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, modifiedAt)
		if err != nil {
			return 0, errors.New("invalid RFC3339 timestamp")
		}
		return parsed.UnixMicro(), nil
	}
	if len(legacy) == 0 || string(legacy) == "null" {
		return 0, nil
	}
	var micros int64
	if err := json.Unmarshal(legacy, &micros); err != nil {
		return 0, errors.New("invalid microsecond timestamp")
	}
	return micros, nil
}
