package anyshare_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
)

func TestUploadControlPlaneWireContracts(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantBody string
		response string
		call     func(*anyshare.Client) (any, error)
		want     any
	}{
		{
			name: "storage options", endpoint: "/api/efast/v1/file/osoption", wantBody: "",
			response: `{"partminsize":204800,"partmaxsize":5368709120,"partmaxnum":10000}`,
			call:     func(c *anyshare.Client) (any, error) { return c.StorageOptions(context.Background()) },
			want:     anyshare.StorageOptions{PartMinSize: 204800, PartMaxSize: 5368709120, PartMaxNum: 10000},
		},
		{
			name: "pre-upload", endpoint: "/api/efast/v1/file/predupload",
			wantBody: `{"length":17,"slice_md5":"abcdef0123456789"}`,
			response: `{"match":true}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.PreUpload(context.Background(), 17, "abcdef0123456789")
			},
			want: true,
		},
		{
			name: "direct new file", endpoint: "/api/efast/v1/file/dupload",
			wantBody: `{"crc32":"89ABCDEF","docid":"gns://parent","length":17,"md5":"ABCDEF0123456789","client_mtime":1767323045000001,"name":"new.bin","ondup":1}`,
			response: `{"docid":"gns://new-file","rev":"new-r1","name":"new.bin","modified":1767323045123456}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.DirectUpload(context.Background(), anyshare.DirectUploadRequest{
					ParentID: "gns://parent", Name: "new.bin", Size: 17, ClientMtimeUS: 1767323045000001,
					Checksums: anyshare.Checksums{MD5: "abcdef0123456789", SliceMD5: "must-not-be-sent", CRC32: "89abcdef"},
				})
			},
			want: anyshare.UploadResult{DocID: "gns://new-file", Rev: "new-r1", Name: "new.bin", Modified: 1767323045123456},
		},
		{
			name: "begin single new file", endpoint: "/api/efast/v1/file/osbeginupload",
			wantBody: `{"docid":"gns://parent","length":23,"name":"new.bin","client_mtime":1767323045000002,"ondup":1,"reqmethod":"PUT"}`,
			response: `{"authrequest":["PUT","https://objects.example/new-file","Authorization: AWS synthetic-new"],"docid":"gns://new-file","name":"new.bin","rev":"upload-r1"}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.BeginSingle(context.Background(), anyshare.BeginRequest{ParentID: "gns://parent", Name: "new.bin", Size: 23, ClientMtimeUS: 1767323045000002})
			},
			want: anyshare.BeginUpload{DocID: "gns://new-file", Rev: "upload-r1", Name: "new.bin", Request: &anyshare.SignedRequest{Method: "PUT", URL: "https://objects.example/new-file", Headers: http.Header{"Authorization": {"AWS synthetic-new"}}}},
		},
		{
			name: "begin single overwrite", endpoint: "/api/efast/v1/file/osbeginupload",
			wantBody: `{"docid":"gns://existing","length":29,"client_mtime":1767323045000003,"reqmethod":"PUT","editedrev":"original-r7"}`,
			response: `{"authrequest":["PUT","https://objects.example/existing","Authorization: AWS synthetic-overwrite"],"docid":"gns://existing","rev":"upload-r8"}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.BeginSingle(context.Background(), anyshare.BeginRequest{ExistingID: "gns://existing", EditedRev: "original-r7", Size: 29, ClientMtimeUS: 1767323045000003})
			},
			want: anyshare.BeginUpload{DocID: "gns://existing", Rev: "upload-r8", Request: &anyshare.SignedRequest{Method: "PUT", URL: "https://objects.example/existing", Headers: http.Header{"Authorization": {"AWS synthetic-overwrite"}}}},
		},
		{
			name: "initialize multipart new file", endpoint: "/api/efast/v1/file/osinitmultiupload",
			wantBody: `{"docid":"gns://parent","length":31,"name":"large.bin","client_mtime":1767323045000004,"ondup":1}`,
			response: `{"docid":"gns://large-file","name":"large.bin","rev":"upload-r1","uploadid":"multipart-1"}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.InitMultipart(context.Background(), anyshare.BeginRequest{ParentID: "gns://parent", Name: "large.bin", Size: 31, ClientMtimeUS: 1767323045000004})
			},
			want: anyshare.BeginUpload{DocID: "gns://large-file", Rev: "upload-r1", Name: "large.bin", UploadID: "multipart-1"},
		},
		{
			name: "initialize multipart overwrite", endpoint: "/api/efast/v1/file/osinitmultiupload",
			wantBody: `{"docid":"gns://existing","length":37,"client_mtime":1767323045000005,"editedrev":"original-r9"}`,
			response: `{"docid":"gns://existing","rev":"upload-r10","uploadid":"multipart-2"}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.InitMultipart(context.Background(), anyshare.BeginRequest{ExistingID: "gns://existing", EditedRev: "original-r9", Size: 37, ClientMtimeUS: 1767323045000005})
			},
			want: anyshare.BeginUpload{DocID: "gns://existing", Rev: "upload-r10", UploadID: "multipart-2"},
		},
		{
			name: "authorize inclusive part range", endpoint: "/api/efast/v1/file/osuploadpart",
			wantBody: `{"docid":"gns://large-file","rev":"upload-r1","uploadid":"multipart-1","parts":"1-4"}`,
			response: `{"1":["PUT","https://objects.example/part-1","Authorization: AWS synthetic-1"],"2":["PUT","https://objects.example/part-2","Authorization: AWS synthetic-2"],"3":["PUT","https://objects.example/part-3","Authorization: AWS synthetic-3"],"4":["PUT","https://objects.example/part-4","Authorization: AWS synthetic-4"]}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.AuthorizeParts(context.Background(), "gns://large-file", "upload-r1", "multipart-1", 1, 4)
			},
			want: anyshare.PartAuthorization{
				1: {Method: "PUT", URL: "https://objects.example/part-1", Headers: http.Header{"Authorization": {"AWS synthetic-1"}}},
				2: {Method: "PUT", URL: "https://objects.example/part-2", Headers: http.Header{"Authorization": {"AWS synthetic-2"}}},
				3: {Method: "PUT", URL: "https://objects.example/part-3", Headers: http.Header{"Authorization": {"AWS synthetic-3"}}},
				4: {Method: "PUT", URL: "https://objects.example/part-4", Headers: http.Header{"Authorization": {"AWS synthetic-4"}}},
			},
		},
		{
			name: "refresh multipart", endpoint: "/api/efast/v1/file/osuploadrefresh",
			wantBody: `{"docid":"gns://large-file","rev":"upload-r1","length":41,"multiupload":true}`,
			response: `{"uploadid":"multipart-3"}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.RefreshUpload(context.Background(), "gns://large-file", "upload-r1", 41, true)
			},
			want: anyshare.RefreshResult{UploadID: "multipart-3"},
		},
		{
			name: "finish new file and retain identity", endpoint: "/api/efast/v1/file/osendupload",
			wantBody: `{"docid":"gns://new-file","rev":"upload-r1","crc32":"89ABCDEF","md5":"ABCDEF0123456789","slice_md5":"0123456789ABCDEF"}`,
			response: `{"name":"new.bin","modified":1767323045123457}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.FinishUpload(context.Background(), anyshare.FinishRequest{DocID: "gns://new-file", Rev: "upload-r1", Checksums: anyshare.Checksums{MD5: "abcdef0123456789", SliceMD5: "0123456789abcdef", CRC32: "89abcdef"}})
			},
			want: anyshare.UploadResult{DocID: "gns://new-file", Rev: "upload-r1", Name: "new.bin", Modified: 1767323045123457},
		},
		{
			name: "finish overwrite with original revision", endpoint: "/api/efast/v1/file/osendupload",
			wantBody: `{"docid":"gns://existing","rev":"upload-r8","crc32":"89ABCDEF","md5":"ABCDEF0123456789","slice_md5":"0123456789ABCDEF","editedrev":"original-r7"}`,
			response: `{}`,
			call: func(c *anyshare.Client) (any, error) {
				return c.FinishUpload(context.Background(), anyshare.FinishRequest{DocID: "gns://existing", Rev: "upload-r8", EditedRev: "original-r7", Checksums: anyshare.Checksums{MD5: "abcdef0123456789", SliceMD5: "0123456789abcdef", CRC32: "89abcdef"}})
			},
			want: anyshare.UploadResult{DocID: "gns://existing", Rev: "upload-r8"},
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
				t.Fatalf("result = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDirectUploadRejectsOverwriteBeforeCredentialsOrNetwork(t *testing.T) {
	tokens := &tokenSourceFake{}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()

	_, err := newTestClient(server, tokens).DirectUpload(context.Background(), anyshare.DirectUploadRequest{
		ExistingID: "gns://existing-secret", EditedRev: "original-secret-r7", Size: 17,
		Checksums: anyshare.Checksums{MD5: "ABCDEF", CRC32: "12345678"},
	})
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
		t.Fatalf("err=%v, want local category", err)
	}
	if len(tokens.calls()) != 0 || requests != 0 {
		t.Fatalf("token calls=%v requests=%d, want no external access", tokens.calls(), requests)
	}
	if strings.Contains(err.Error(), "gns://existing-secret") || strings.Contains(err.Error(), "original-secret-r7") {
		t.Fatalf("error leaked rejected overwrite input: %v", err)
	}
}

func TestNonIdempotentUploadOutcomeClassification(t *testing.T) {
	// Production mutation caught: erasing definite 4xx rejection versus unknown 5xx/malformed-success outcome at the AnyShare boundary.
	tests := []struct {
		name        string
		status      int
		body        string
		wantUnknown bool
	}{
		{name: "precondition is definite", status: http.StatusPreconditionFailed, body: `{"message":"synthetic precondition"}`},
		{name: "server failure is unknown", status: http.StatusServiceUnavailable, body: `{"message":"synthetic unavailable"}`, wantUnknown: true},
		{name: "malformed success is unknown", status: http.StatusOK, body: `{`, wantUnknown: true},
		{name: "incomplete success is unknown", status: http.StatusOK, body: `{}`, wantUnknown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			_, err := newTestClient(server, staticToken("fixture-access")).DirectUpload(context.Background(), anyshare.DirectUploadRequest{
				ParentID: "gns://parent", Name: "fixture.bin", Size: 1, Checksums: anyshare.Checksums{MD5: "AA", CRC32: "BB"},
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if got := anyshare.IsOutcomeUnknown(err); got != tt.wantUnknown {
				t.Fatalf("unknown=%v want=%v err=%v", got, tt.wantUnknown, err)
			}
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
				t.Fatalf("err=%v category=%v", err, appErr)
			}
		})
	}
}

func TestNonIdempotentTokenFailureIsDefinitelyNotSent(t *testing.T) {
	// Production mutation caught: classifying token acquisition failure before HTTP dispatch as outcome-unknown and permanently blocking safe retry.
	tests := []struct {
		name   string
		tokens interface {
			Token(context.Context, bool) (string, error)
		}
		status   int
		wantCat  apperr.Category
		wantSent int
	}{
		{name: "initial network", tokens: errorToken{err: apperr.Wrap(apperr.Network, "fixture", "token unavailable", errors.New("synthetic token failure"))}, wantCat: apperr.Network},
		{name: "initial interrupted", tokens: errorToken{err: context.Canceled}, wantCat: apperr.Interrupted},
		{name: "refresh network", tokens: refreshFailureToken{err: apperr.Wrap(apperr.Network, "fixture", "refresh unavailable", errors.New("synthetic refresh failure"))}, status: http.StatusUnauthorized, wantCat: apperr.Network, wantSent: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			_, err := newTestClient(server, tt.tokens).DirectUpload(context.Background(), anyshare.DirectUploadRequest{
				ParentID: "gns://parent", Name: "fixture.bin", Size: 1, Checksums: anyshare.Checksums{MD5: "AA", CRC32: "BB"},
			})
			if err == nil {
				t.Fatal("expected token failure")
			}
			if anyshare.IsOutcomeUnknown(err) {
				t.Fatalf("pre-dispatch failure marked outcome unknown: %v", err)
			}
			if !anyshare.IsRequestNotSent(err) {
				t.Fatalf("pre-dispatch failure missing request-not-sent marker: %v", err)
			}
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != tt.wantCat {
				t.Fatalf("err=%v category=%v want=%v", err, appErr, tt.wantCat)
			}
			if requests != tt.wantSent {
				t.Fatalf("requests=%d want=%d", requests, tt.wantSent)
			}
		})
	}
}

type refreshFailureToken struct{ err error }

func (t refreshFailureToken) Token(_ context.Context, forceRefresh bool) (string, error) {
	if forceRefresh {
		return "", t.err
	}
	return "fixture-access", nil
}

func TestBeginUploadRejectsMalformedIdentityBeforeCredentialsOrNetwork(t *testing.T) {
	tests := []struct {
		name string
		req  anyshare.BeginRequest
		call func(*anyshare.Client, anyshare.BeginRequest) (anyshare.BeginUpload, error)
	}{
		{name: "new missing parent", req: anyshare.BeginRequest{Name: "new.bin", Size: 1}, call: func(c *anyshare.Client, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
			return c.BeginSingle(context.Background(), req)
		}},
		{name: "new missing name", req: anyshare.BeginRequest{ParentID: "gns://parent", Size: 1}, call: func(c *anyshare.Client, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
			return c.InitMultipart(context.Background(), req)
		}},
		{name: "overwrite missing original revision", req: anyshare.BeginRequest{ExistingID: "gns://existing", Size: 1}, call: func(c *anyshare.Client, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
			return c.BeginSingle(context.Background(), req)
		}},
		{name: "new with stray edited revision", req: anyshare.BeginRequest{ParentID: "gns://parent", Name: "new.bin", EditedRev: "original-r1", Size: 1}, call: func(c *anyshare.Client, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
			return c.InitMultipart(context.Background(), req)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := &tokenSourceFake{}
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			_, err := tt.call(newTestClient(server, tokens), tt.req)
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
				t.Fatalf("err=%v, want local category", err)
			}
			if len(tokens.calls()) != 0 || requests != 0 {
				t.Fatalf("token calls=%v requests=%d, want no external access", tokens.calls(), requests)
			}
		})
	}
}

func TestOverwriteInitializationRejectsUnexpectedIdentity(t *testing.T) {
	// Production mutation caught: returning a signed overwrite authorization for another document or the unchanged original revision to the object-write layer.
	tests := []struct {
		name     string
		endpoint string
		response string
		call     func(*anyshare.Client) (anyshare.BeginUpload, error)
	}{
		{
			name: "single different document", endpoint: "/api/efast/v1/file/osbeginupload",
			response: `{"authrequest":["PUT","https://objects.example/other","Authorization: AWS synthetic"],"docid":"gns://other","rev":"upload-r8"}`,
			call: func(c *anyshare.Client) (anyshare.BeginUpload, error) {
				return c.BeginSingle(context.Background(), anyshare.BeginRequest{ExistingID: "gns://existing", EditedRev: "original-r7", Size: 1})
			},
		},
		{
			name: "multipart original revision", endpoint: "/api/efast/v1/file/osinitmultiupload",
			response: `{"docid":"gns://existing","rev":"original-r7","uploadid":"multipart-1"}`,
			call: func(c *anyshare.Client) (anyshare.BeginUpload, error) {
				return c.InitMultipart(context.Background(), anyshare.BeginRequest{ExistingID: "gns://existing", EditedRev: "original-r7", Size: 1})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.endpoint {
					t.Fatalf("path=%q want=%q", r.URL.Path, tt.endpoint)
				}
				_, _ = io.WriteString(w, tt.response)
			}))
			defer server.Close()
			_, err := tt.call(newTestClient(server, staticToken("fixture-access")))
			if err == nil || !anyshare.IsOutcomeUnknown(err) {
				t.Fatalf("err=%v, want outcome-unknown identity rejection", err)
			}
		})
	}
}

func TestUploadInitializationRejectsMissingOrConflictingResponseVariant(t *testing.T) {
	tests := []struct {
		name     string
		response string
		call     func(*anyshare.Client) error
	}{
		{name: "single begin missing request", response: `{"docid":"gns://file","rev":"upload-r1"}`, call: func(c *anyshare.Client) error {
			_, err := c.BeginSingle(context.Background(), anyshare.BeginRequest{ParentID: "gns://parent", Name: "new.bin", Size: 1})
			return err
		}},
		{name: "multipart init missing upload ID", response: `{"docid":"gns://file","rev":"upload-r1"}`, call: func(c *anyshare.Client) error {
			_, err := c.InitMultipart(context.Background(), anyshare.BeginRequest{ParentID: "gns://parent", Name: "new.bin", Size: 1})
			return err
		}},
		{name: "single refresh missing request", response: `{}`, call: func(c *anyshare.Client) error {
			_, err := c.RefreshUpload(context.Background(), "gns://file", "upload-r1", 1, false)
			return err
		}},
		{name: "multipart refresh missing upload ID", response: `{}`, call: func(c *anyshare.Client) error {
			_, err := c.RefreshUpload(context.Background(), "gns://file", "upload-r1", 1, true)
			return err
		}},
		{name: "multipart refresh conflicting variants", response: `{"uploadid":"multipart-2","authrequest":["PUT","https://objects.example/item","Authorization: AWS synthetic"]}`, call: func(c *anyshare.Client) error {
			_, err := c.RefreshUpload(context.Background(), "gns://file", "upload-r1", 1, true)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tt.response) }))
			defer server.Close()
			err := tt.call(newTestClient(server, staticToken("fixture-access")))
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
				t.Fatalf("err=%v, want remote category", err)
			}
		})
	}
}

func TestAuthorizePartsRejectsOutOfRangeBeforeCredentialsOrNetwork(t *testing.T) {
	tests := []struct{ first, last int }{{0, 1}, {1, 10001}, {4, 3}}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("range-%d-%d", tt.first, tt.last), func(t *testing.T) {
			tokens := &tokenSourceFake{}
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			_, err := newTestClient(server, tokens).AuthorizeParts(context.Background(), "gns://doc", "rev", "upload", tt.first, tt.last)
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
				t.Fatalf("err=%v, want local category", err)
			}
			if len(tokens.calls()) != 0 || requests != 0 {
				t.Fatalf("token calls=%v requests=%d, want no external access", tokens.calls(), requests)
			}
		})
	}
}

func TestAuthorizePartsRejectsIncompleteNoncanonicalOrDuplicateResponses(t *testing.T) {
	const request = `["PUT","https://objects.example/part","Authorization: AWS synthetic-part"]`
	tests := []struct {
		name     string
		first    int
		last     int
		response string
	}{
		{name: "empty", first: 1, last: 1, response: `{}`},
		{name: "missing middle", first: 1, last: 3, response: `{"1":` + request + `,"3":` + request + `}`},
		{name: "noncanonical alias", first: 1, last: 1, response: `{"1":` + request + `,"01":` + request + `}`},
		{name: "duplicate exact key", first: 1, last: 1, response: `{"1":` + request + `,"1":` + request + `}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tt.response)
			}))
			defer server.Close()

			_, err := newTestClient(server, staticToken("fixture-access")).AuthorizeParts(context.Background(), "gns://file", "upload-r1", "multipart-1", tt.first, tt.last)
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
				t.Fatalf("err=%v, want remote category", err)
			}
		})
	}
}

func TestCompleteMultipartOrdersPartsNumericallyAndPreservesXML(t *testing.T) {
	fixture := readFixture(t, "complete_multipart_response.txt")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/oscompleteupload")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		wantBody := `{"docid":"gns://large-file","rev":"upload-r1","uploadid":"multipart-1","partinfo":{"1":["\"etag-one\"",11],"2":["etag-two",22],"10":["etag-ten",1010]}}`
		if string(body) != wantBody {
			t.Fatalf("body = %s, want %s", body, wantBody)
		}
		w.Header().Set("Content-Type", "multipart/form-data; boundary=synthetic-boundary")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	signed, completionXML, err := newTestClient(server, staticToken("fixture-access")).CompleteMultipart(context.Background(), "gns://large-file", "upload-r1", "multipart-1", map[int]anyshare.PartInfo{
		10: {ETag: "etag-ten", Size: 1010},
		1:  {ETag: `"etag-one"`, Size: 11},
		2:  {ETag: "etag-two", Size: 22},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantXML := []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"synthetic-one"</ETag></Part><Part><PartNumber>10</PartNumber><ETag>"synthetic-ten"</ETag></Part></CompleteMultipartUpload>`)
	if !reflect.DeepEqual(completionXML, wantXML) {
		t.Fatalf("completion XML = %q, want byte-exact %q", completionXML, wantXML)
	}
	wantSigned := anyshare.SignedRequest{Method: "PUT", URL: "https://objects.example/complete?signature=synthetic-completion-query", Headers: http.Header{
		"Authorization": {"AWS synthetic-completion-authorization"},
		"Content-Type":  {"application/xml"},
	}}
	if !reflect.DeepEqual(signed, wantSigned) {
		t.Fatalf("signed request = %#v, want %#v", signed, wantSigned)
	}
}

func TestCompleteMultipartRejectsMalformedResponseWithoutLeakingBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/form-data; boundary=synthetic-boundary")
		_, _ = io.WriteString(w, "malformed-completion-secret signature=malformed-query-secret")
	}))
	defer server.Close()

	_, _, err := newTestClient(server, staticToken("fixture-access")).CompleteMultipart(context.Background(), "gns://file", "rev", "upload", map[int]anyshare.PartInfo{1: {ETag: "etag", Size: 1}})
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err=%v, want remote category", err)
	}
	for _, marker := range []string{"malformed-completion-secret", "malformed-query-secret"} {
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error leaked multipart response marker %q: %v", marker, err)
		}
	}
}

func TestCompleteMultipartRejectsUnknownEmptyOrMislabeledMIMEParts(t *testing.T) {
	tests := []struct {
		name      string
		xmlType   string
		xmlBody   string
		jsonType  string
		jsonBody  string
		extraPart string
	}{
		{name: "missing XML content type", xmlBody: "<Complete>xml-secret</Complete>", jsonType: "application/json", jsonBody: `{"authrequest":["PUT","https://objects.example/complete","Authorization: AWS synthetic"]}`},
		{name: "wrong XML content type", xmlType: "text/plain", xmlBody: "<Complete>xml-secret</Complete>", jsonType: "application/json", jsonBody: `{"authrequest":["PUT","https://objects.example/complete","Authorization: AWS synthetic"]}`},
		{name: "empty XML", xmlType: "application/xml", jsonType: "application/json", jsonBody: `{"authrequest":["PUT","https://objects.example/complete","Authorization: AWS synthetic"]}`},
		{name: "missing JSON content type", xmlType: "application/xml", xmlBody: "<Complete>xml-secret</Complete>", jsonBody: `{"authrequest":["PUT","https://objects.example/complete","Authorization: AWS synthetic"]}`},
		{name: "wrong JSON content type", xmlType: "application/xml", xmlBody: "<Complete>xml-secret</Complete>", jsonType: "text/plain", jsonBody: `{"authrequest":["PUT","https://objects.example/complete","Authorization: AWS synthetic"]}`},
		{name: "unknown extra part", xmlType: "application/xml", xmlBody: "<Complete>xml-secret</Complete>", jsonType: "application/json", jsonBody: `{"authrequest":["PUT","https://objects.example/complete","Authorization: AWS synthetic"]}`, extraPart: "--mime-boundary\r\n" + mimeTestPart("extra", "application/octet-stream", "unknown-part-secret")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := "--mime-boundary\r\n" + mimeTestPart("completion", tt.xmlType, tt.xmlBody) +
				"--mime-boundary\r\n" + mimeTestPart("authrequest", tt.jsonType, tt.jsonBody) + tt.extraPart + "--mime-boundary--\r\n"
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "multipart/form-data; boundary=mime-boundary")
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()

			_, _, err := newTestClient(server, staticToken("fixture-access")).CompleteMultipart(context.Background(), "gns://file", "rev", "upload", map[int]anyshare.PartInfo{1: {ETag: "etag", Size: 1}})
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
				t.Fatalf("err=%v, want remote category", err)
			}
			for _, marker := range []string{"xml-secret", "unknown-part-secret"} {
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("error leaked multipart body marker %q: %v", marker, err)
				}
			}
		})
	}
}

func mimeTestPart(name, contentType, body string) string {
	part := "Content-Disposition: form-data; name=\"" + name + "\"\r\n"
	if contentType != "" {
		part += "Content-Type: " + contentType + "\r\n"
	}
	return part + "\r\n" + body + "\r\n"
}
