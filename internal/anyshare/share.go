package anyshare

import (
	"context"
	"net/http"
)

// EntryItems returns the document roots exposed to an anonymous shared-link
// session. Authentication is supplied by the short-lived link token used to
// construct the Client.
func (c *Client) EntryItems(ctx context.Context) ([]Item, error) {
	var wireItems []itemWire
	if err := c.callJSON(ctx, http.MethodGet, "/api/efast/v1/entry-item", nil, true, &wireItems); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(wireItems))
	for _, wireItem := range wireItems {
		stableType := wireItem.Type
		if stableType == "folder" {
			stableType = "directory"
		}
		item, err := normalizeItem(wireItem, stableType)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}
