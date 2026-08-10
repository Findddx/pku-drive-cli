package anyshare_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
)

func TestEntryDocLibsUsesSortedEndpointAndNormalizesPaths(t *testing.T) {
	fixture := readFixture(t, "entry_doc_libs.json")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodGet, "/api/efast/v1/entry-doc-lib?direction=asc&sort=doc_lib_name")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	items, err := newTestClient(server, staticToken("fixture-access")).EntryDocLibs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []anyshare.Item{
		{ID: "gns://library-a", Name: "Library A", Path: "/Library A", Type: "user_doc_lib", Rev: "lib-r1"},
		{ID: "gns://library-b", Name: "共享 + 资料", Path: "/共享 + 资料", Type: "shared_doc_lib", Rev: "lib-r2"},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items = %#v, want %#v", items, want)
	}
}

func TestListFolderPageEncodesIDAndMarkerOnceAndNormalizesWireItems(t *testing.T) {
	fixture := readFixture(t, "folder_page_1.json")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodGet, "/api/efast/v1/folders/gns:%2F%2Flib%2F%E8%B5%84%E6%96%99%20+/sub_objects?limit=1000&marker=%E9%A1%B5%2F2+%2B")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	page, err := newTestClient(server, staticToken("fixture-access")).ListFolderPage(context.Background(), "gns://lib/资料 +", "页/2 +")
	if err != nil {
		t.Fatal(err)
	}
	want := anyshare.Page{
		Entries: []anyshare.Item{
			{ID: "gns://legacy-dir", DocID: "gns://legacy-dir", Name: "Old Folder", Type: "directory", Rev: "dir-r1", Modified: 1767323045123456, ClientMtimeUS: 1767323045000000},
			{ID: "gns://new-file", Name: "New File.txt", Type: "file", Rev: "file-r1", Size: 17, Modified: 1767323045123456, ClientMtimeUS: 1767323045999999},
		},
		Marker: "页/2 +",
	}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("page = %#v, want %#v", page, want)
	}
}

func TestListFolderFollowsPagesAndKeepsDirsBeforeFiles(t *testing.T) {
	page1 := readFixture(t, "folder_page_1.json")
	page2 := readFixture(t, "folder_page_2.json")
	var mu sync.Mutex
	var requests []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.RequestURI)
		mu.Unlock()
		switch r.RequestURI {
		case "/api/efast/v1/folders/gns:%2F%2Flib%2F%E8%B5%84%E6%96%99%20+/sub_objects?limit=1000":
			_, _ = w.Write(page1)
		case "/api/efast/v1/folders/gns:%2F%2Flib%2F%E8%B5%84%E6%96%99%20+/sub_objects?limit=1000&marker=%E9%A1%B5%2F2+%2B":
			_, _ = w.Write(page2)
		default:
			t.Fatalf("unexpected request URI %q", r.RequestURI)
		}
	}))
	defer server.Close()

	items, err := newTestClient(server, staticToken("fixture-access")).ListFolder(context.Background(), "gns://lib/资料 +")
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"Old Folder", "New File.txt", "New Folder", "Old File.bin"}
	gotNames := make([]string, len(items))
	for i := range items {
		gotNames[i] = items[i].Name
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("names = %q, want %q", gotNames, wantNames)
	}
	wantRequests := []string{
		"/api/efast/v1/folders/gns:%2F%2Flib%2F%E8%B5%84%E6%96%99%20+/sub_objects?limit=1000",
		"/api/efast/v1/folders/gns:%2F%2Flib%2F%E8%B5%84%E6%96%99%20+/sub_objects?limit=1000&marker=%E9%A1%B5%2F2+%2B",
	}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("requests = %q, want %q", requests, wantRequests)
	}
}

func TestListFolderRejectsRepeatedMarker(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"dirs":[],"files":[],"next_marker":"same marker"}`)
	}))
	defer server.Close()

	_, err := newTestClient(server, staticToken("fixture-access")).ListFolder(context.Background(), "gns://library")
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err=%v, want remote category", err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want 2 before repeated-marker rejection", requests)
	}
}

func TestDocumentPOSTEndpointsUseExactBodiesAndNormalizeResponses(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantBody string
		response string
		call     func(*anyshare.Client) (anyshare.Item, error)
		want     anyshare.Item
	}{
		{
			name: "resolve name path", endpoint: "/api/efast/v1/file/getinfobypath",
			wantBody: `{"namepath":"/Library A/共享 + 文档.txt"}`,
			response: `{"docid":"gns://resolved-file","name":"共享 + 文档.txt","type":"file","rev":"resolved-r1","size":29,"modified":1767323045123456,"client_mtime":1767323045000002}`,
			call: func(c *anyshare.Client) (anyshare.Item, error) {
				return c.ResolveNamePath(context.Background(), "/Library A/共享 + 文档.txt")
			},
			want: anyshare.Item{ID: "gns://resolved-file", DocID: "gns://resolved-file", Name: "共享 + 文档.txt", Type: "file", Rev: "resolved-r1", Size: 29, Modified: 1767323045123456, ClientMtimeUS: 1767323045000002},
		},
		{
			name: "create directory", endpoint: "/api/efast/v1/dir/create",
			wantBody: `{"docid":"gns://parent","name":"child","ondup":1}`,
			response: `{"id":"gns://child","name":"Server Child","rev":"child-r1","modified_at":"2026-01-02T03:04:05.123456Z"}`,
			call: func(c *anyshare.Client) (anyshare.Item, error) {
				return c.CreateDir(context.Background(), "gns://parent", "child")
			},
			want: anyshare.Item{ID: "gns://child", Name: "Server Child", Type: "directory", Rev: "child-r1", Modified: 1767323045123456},
		},
		{
			name: "file metadata", endpoint: "/api/efast/v1/file/metadata",
			wantBody: `{"docid":"gns://metadata-file","rev":"metadata-r3"}`,
			response: `{"docid":"gns://metadata-file","name":"metadata.bin","rev":"metadata-r3","size":41,"modified":1767323045123456,"client_mtime":1767323045000003,"md5":"0123456789ABCDEF","slice_md5":"ABCDEF0123456789","crc32":"89ABCDEF"}`,
			call: func(c *anyshare.Client) (anyshare.Item, error) {
				return c.FileMetadata(context.Background(), "gns://metadata-file", "metadata-r3")
			},
			want: anyshare.Item{ID: "gns://metadata-file", DocID: "gns://metadata-file", Name: "metadata.bin", Type: "file", Rev: "metadata-r3", Size: 41, Modified: 1767323045123456, ClientMtimeUS: 1767323045000003, MD5: "0123456789ABCDEF", SliceMD5: "ABCDEF0123456789", CRC32: "89ABCDEF"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertAuthorizedRequest(t, r, http.MethodPost, tt.endpoint)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != tt.wantBody {
					t.Fatalf("body = %s, want %s", body, tt.wantBody)
				}
				_, _ = io.WriteString(w, tt.response)
			}))
			defer server.Close()

			got, err := tt.call(newTestClient(server, staticToken("fixture-access")))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("item = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCreateDirFillsNameMissingFromDocumentedResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/dir/create")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"docid":"gns://parent","name":"child","ondup":1}` {
			t.Fatalf("body = %s", body)
		}
		_, _ = io.WriteString(w, `{"docid":"gns://child","rev":"child-r1","modified":1767323045123456,"create_time":1767323045000000,"creator":"fixture-creator","editor":"fixture-editor"}`)
	}))
	defer server.Close()

	item, err := newTestClient(server, staticToken("fixture-access")).CreateDir(context.Background(), "gns://parent", "child")
	if err != nil {
		t.Fatal(err)
	}
	want := anyshare.Item{ID: "gns://child", DocID: "gns://child", Name: "child", Type: "directory", Rev: "child-r1", Modified: 1767323045123456}
	if !reflect.DeepEqual(item, want) {
		t.Fatalf("item = %#v, want %#v", item, want)
	}
}

func TestResolveNamePathInfersMissingTypeFromSize(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		body     string
		response string
		wantType string
	}{
		{
			name:     "documented directory sentinel",
			path:     "/Library A/Folder",
			body:     `{"namepath":"/Library A/Folder"}`,
			response: `{"docid":"gns://folder","name":"Folder","rev":"dir-r1","size":-1,"modified":1767323045123456}`,
			wantType: "directory",
		},
		{
			name:     "zero size remains file",
			path:     "/Library A/empty.bin",
			body:     `{"namepath":"/Library A/empty.bin"}`,
			response: `{"docid":"gns://empty-file","name":"empty.bin","rev":"file-r1","size":0,"modified":1767323045123456}`,
			wantType: "file",
		},
		{
			name:     "explicit type is preserved",
			path:     "/Library A/explicit",
			body:     `{"namepath":"/Library A/explicit"}`,
			response: `{"docid":"gns://explicit","name":"explicit","type":"official-kind","rev":"explicit-r1","size":-1,"modified":1767323045123456}`,
			wantType: "official-kind",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/getinfobypath")
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != tt.body {
					t.Fatalf("body = %s, want %s", body, tt.body)
				}
				_, _ = io.WriteString(w, tt.response)
			}))
			defer server.Close()

			item, err := newTestClient(server, staticToken("fixture-access")).ResolveNamePath(context.Background(), tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if item.Type != tt.wantType {
				t.Fatalf("Type = %q, want %q (size=%d)", item.Type, tt.wantType, item.Size)
			}
		})
	}
}

func TestDeleteUsesExactEndpointBodyAndStatus(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		body       string
		statusCode int
		wantStatus anyshare.DeleteStatus
		call       func(*anyshare.Client) (anyshare.DeleteResult, error)
	}{
		{
			name: "file deleted", endpoint: "/api/efast/v1/file/delete",
			body: `{"docid":"gns://exact-file-id"}`, statusCode: http.StatusOK,
			wantStatus: anyshare.DeleteStatusDeleted,
			call: func(client *anyshare.Client) (anyshare.DeleteResult, error) {
				return client.DeleteFile(context.Background(), "gns://exact-file-id")
			},
		},
		{
			name: "file pending review", endpoint: "/api/efast/v1/file/delete",
			body: `{"docid":"gns://exact-file-id"}`, statusCode: http.StatusAccepted,
			wantStatus: anyshare.DeleteStatusPendingReview,
			call: func(client *anyshare.Client) (anyshare.DeleteResult, error) {
				return client.DeleteFile(context.Background(), "gns://exact-file-id")
			},
		},
		{
			name: "directory deleted with upload guard", endpoint: "/api/efast/v1/dir/delete",
			body: `{"docid":"gns://exact-directory-id","check_upload_process":true}`, statusCode: http.StatusOK,
			wantStatus: anyshare.DeleteStatusDeleted,
			call: func(client *anyshare.Client) (anyshare.DeleteResult, error) {
				return client.DeleteDir(context.Background(), "gns://exact-directory-id")
			},
		},
		{
			name: "directory pending review", endpoint: "/api/efast/v1/dir/delete",
			body: `{"docid":"gns://exact-directory-id","check_upload_process":true}`, statusCode: http.StatusAccepted,
			wantStatus: anyshare.DeleteStatusPendingReview,
			call: func(client *anyshare.Client) (anyshare.DeleteResult, error) {
				return client.DeleteDir(context.Background(), "gns://exact-directory-id")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				assertAuthorizedRequest(t, request, http.MethodPost, test.endpoint)
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != test.body {
					t.Fatalf("body = %s, want %s", body, test.body)
				}
				w.WriteHeader(test.statusCode)
				_, _ = io.WriteString(w, `{"ignored":"response body"}`)
			}))
			defer server.Close()

			result, err := test.call(newTestClient(server, staticToken("fixture-access")))
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
		})
	}
}

func TestDeleteRejectsUnexpectedSuccessfulStatus(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	_, err := newTestClient(server, staticToken("fixture-access")).DeleteFile(context.Background(), "gns://file")
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err = %v, want remote error", err)
	}
}

func TestFileMetadataOmitsEmptyRevisionForLatest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/metadata")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"docid":"gns://metadata-file"}` {
			t.Fatalf("body = %s, want docid-only latest-revision request", body)
		}
		_, _ = io.WriteString(w, `{"docid":"gns://metadata-file","name":"metadata.bin","rev":"latest-r4","size":0,"modified":1767323045123456}`)
	}))
	defer server.Close()

	item, err := newTestClient(server, staticToken("fixture-access")).FileMetadata(context.Background(), "gns://metadata-file", "")
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "gns://metadata-file" || item.Rev != "latest-r4" || item.Type != "file" {
		t.Fatalf("item = %#v", item)
	}
}

func TestMutatingDocumentCallsDoNotTransportRetry(t *testing.T) {
	tests := []struct {
		name string
		call func(*anyshare.Client) error
	}{
		{name: "create", call: func(c *anyshare.Client) error {
			_, err := c.CreateDir(context.Background(), "gns://parent", "child")
			return err
		}},
		{name: "delete", call: func(c *anyshare.Client) error {
			_, err := c.DeleteDir(context.Background(), "gns://child")
			return err
		}},
		{name: "delete file", call: func(c *anyshare.Client) error {
			_, err := c.DeleteFile(context.Background(), "gns://child")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			client := anyshare.NewClient(server.URL, httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 3}), staticToken("fixture-access"))
			if err := tt.call(client); err == nil {
				t.Fatal("error=nil, want remote error")
			}
			if requests != 1 {
				t.Fatalf("requests=%d, want 1 for non-idempotent mutation", requests)
			}
		})
	}
}

func TestReadOnlyPOSTMayTransportRetry(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"id":"gns://file","name":"file"}`)
	}))
	defer server.Close()
	transport := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 2})
	transport.Sleep = func(context.Context, time.Duration) error { return nil }
	client := anyshare.NewClient(server.URL, transport, staticToken("fixture-access"))

	if _, err := client.ResolveNamePath(context.Background(), "/Library/file"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want 2 for read-only POST retry", requests)
	}
}

func assertAuthorizedRequest(t *testing.T, r *http.Request, method, requestURI string) {
	t.Helper()
	if r.Method != method || r.RequestURI != requestURI {
		t.Fatalf("request = %s %s, want %s %s", r.Method, r.RequestURI, method, requestURI)
	}
	if r.Header.Get("Authorization") != "Bearer fixture-access" {
		t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
	}
	if strings.Contains(r.RequestURI, "fixture-access") {
		t.Fatalf("request URI leaked bearer token: %s", r.RequestURI)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
