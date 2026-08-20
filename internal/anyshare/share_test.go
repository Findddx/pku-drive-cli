package anyshare_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
)

func TestEntryItemsUsesAnonymousEntryEndpointAndNormalizesItems(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodGet, "/api/efast/v1/entry-item")
		_, _ = io.WriteString(w, `[
			{"id":"gns://shared-folder","name":"Shared Folder","type":"folder","rev":"folder-r1","size":-1,"modified_at":"2026-01-02T03:04:05.123456Z"},
			{"docid":"gns://shared-file","name":"report.tsv","type":"file","rev":"file-r1","size":17,"modified":1767323045123456}
		]`)
	}))
	defer server.Close()

	items, err := newTestClient(server, staticToken("fixture-access")).EntryItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []anyshare.Item{
		{ID: "gns://shared-folder", Name: "Shared Folder", Type: "directory", Rev: "folder-r1", Size: -1, Modified: 1767323045123456},
		{ID: "gns://shared-file", DocID: "gns://shared-file", Name: "report.tsv", Type: "file", Rev: "file-r1", Size: 17, Modified: 1767323045123456},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items = %#v, want %#v", items, want)
	}
}
