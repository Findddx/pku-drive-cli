package anyshare

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

const downloadAuthorizationType = "QUERY_STRING"

// DownloadStream is a successful, isolated object-store response. The caller
// owns Body and must close it.
type DownloadStream struct {
	Body          io.ReadCloser
	ContentLength int64
}

// AuthorizeDownload obtains a short-lived signed HTTPS GET for one exact file
// revision. The display name is sent to AnyShare only as the suggested saved
// name; it is never interpreted as a local path here.
func (c *Client) AuthorizeDownload(ctx context.Context, docID, rev, saveName string) (SignedRequest, error) {
	if docID == "" || saveName == "" {
		return SignedRequest{}, wrapError(apperr.Local, "anyshare", "invalid download authorization request", errors.New("missing file identity or name"))
	}
	body, err := marshalBody(struct {
		DocID    string `json:"docid"`
		AuthType string `json:"authtype"`
		SaveName string `json:"savename"`
		UseHTTPS bool   `json:"usehttps"`
		Rev      string `json:"rev"`
	}{DocID: docID, AuthType: downloadAuthorizationType, SaveName: saveName, UseHTTPS: true, Rev: rev})
	if err != nil {
		return SignedRequest{}, err
	}
	var response struct {
		AuthRequest json.RawMessage `json:"authrequest"`
	}
	if err := c.callJSON(ctx, http.MethodPost, "/api/efast/v1/file/osdownload", body, true, &response); err != nil {
		return SignedRequest{}, err
	}
	signed, err := parseDownloadSignedRequest(response.AuthRequest)
	if err != nil {
		return SignedRequest{}, wrapError(apperr.Remote, "anyshare", "invalid download authorization response", errors.New("signed request rejected"))
	}
	return signed, nil
}

// GetSigned opens an isolated object-store response. It sends no OAuth token,
// disables transparent compression, applies the configured object SPKI pin,
// and rejects every redirect so signed authority cannot migrate to a new URL.
func (c *Client) GetSigned(ctx context.Context, signed SignedRequest) (DownloadStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.httpClient == nil || c.httpClient.HTTP == nil {
		return DownloadStream{}, wrapError(apperr.Local, "anyshare", "invalid signed request", errors.New("object-store transport is not configured"))
	}
	validated, err := validateSignedRequest(signed)
	if err != nil || validated.Method != http.MethodGet || len(validated.Headers) != 0 {
		return DownloadStream{}, wrapError(apperr.Local, "anyshare", "invalid signed request", errors.New("signed request rejected"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, validated.URL, nil)
	if err != nil {
		return DownloadStream{}, wrapError(apperr.Local, "anyshare", "invalid signed request", errors.New("signed request rejected"))
	}
	// QUERY_STRING authorization is carried entirely by the URL. Setting an
	// explicit nil value also suppresses Go's default User-Agent.
	req.Header["User-Agent"] = nil

	isolated := *c.httpClient
	standardClient := *c.httpClient.HTTP
	if transport, ok := standardClient.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.DisableCompression = true
		standardClient.Transport = transport
	}
	if err := configurePinnedObjectTransport(&standardClient, req.URL.Hostname(), c.server); err != nil {
		return DownloadStream{}, wrapError(apperr.Local, "anyshare", "invalid object-store trust configuration", errors.New("object-store trust configuration rejected"))
	}
	standardClient.CheckRedirect = signedRedirectPolicy
	isolated.HTTP = &standardClient
	resp, err := isolated.Do(ctx, req, true)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return DownloadStream{}, wrapError(apperr.Interrupted, "anyshare", "object-store request interrupted", contextErr)
		}
		category := apperr.Network
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			category = appErr.Category
		}
		return DownloadStream{}, wrapError(category, "anyshare", "object-store request failed", errors.New("transport error"))
	}
	if resp.StatusCode != http.StatusOK {
		return DownloadStream{}, signedResponseError(resp)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		discardSignedBody(resp.Body)
		return DownloadStream{}, wrapError(apperr.Remote, "anyshare", "invalid object-store response", errors.New("encoded download response rejected"))
	}
	return DownloadStream{Body: resp.Body, ContentLength: resp.ContentLength}, nil
}
