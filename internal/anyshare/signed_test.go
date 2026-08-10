package anyshare_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
)

func TestBeginSingleParsesLegacySignedPUT(t *testing.T) {
	objectServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer objectServer.Close()
	signedURL := objectServer.URL + "/item?signature=synthetic-signature"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/osbeginupload")
		_, _ = fmt.Fprintf(w, `{"authrequest":["PUT",%q,"Content-Type: application/octet-stream","X-Meta: value:with:colons","Authorization: AWS synthetic-authorization"],"docid":"gns://new-file","name":"new.bin","rev":"upload-r1"}`, signedURL)
	}))
	defer server.Close()

	got, err := newTestClient(server, staticToken("fixture-access")).BeginSingle(context.Background(), anyshare.BeginRequest{ParentID: "gns://parent", Name: "new.bin", Size: 17})
	if err != nil {
		t.Fatal(err)
	}
	want := &anyshare.SignedRequest{Method: http.MethodPut, URL: signedURL, Headers: http.Header{
		"Authorization": {"AWS synthetic-authorization"},
		"Content-Type":  {"application/octet-stream"},
		"X-Meta":        {"value:with:colons"},
	}}
	if !reflect.DeepEqual(got.Request, want) {
		t.Fatalf("request = %#v, want %#v", got.Request, want)
	}
}

func TestAuthorizePartsParsesDecimalPartKeys(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/osuploadpart")
		_, _ = io.WriteString(w, `{"9":["PUT","https://objects.example/part-9","Authorization: AWS part-nine"],"10":["PUT","https://objects.example/part-10","Authorization: AWS part-ten"]}`)
	}))
	defer server.Close()

	got, err := newTestClient(server, staticToken("fixture-access")).AuthorizeParts(context.Background(), "gns://file", "upload-r1", "multipart-1", 9, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := anyshare.PartAuthorization{
		9:  {Method: "PUT", URL: "https://objects.example/part-9", Headers: http.Header{"Authorization": {"AWS part-nine"}}},
		10: {Method: "PUT", URL: "https://objects.example/part-10", Headers: http.Header{"Authorization": {"AWS part-ten"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authorization = %#v, want %#v", got, want)
	}
}

func TestAuthorizePartsParsesDeployedAuthRequestsEnvelope(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/osuploadpart")
		_, _ = io.WriteString(w, `{"authrequests":{"1":["PUT","https://objects.example/part-1","Authorization: AWS part-one"],"2":["PUT","https://objects.example/part-2","Authorization: AWS part-two"]}}`)
	}))
	defer server.Close()

	got, err := newTestClient(server, staticToken("fixture-access")).AuthorizeParts(context.Background(), "gns://file", "upload-r1", "multipart-1", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := anyshare.PartAuthorization{
		1: {Method: "PUT", URL: "https://objects.example/part-1", Headers: http.Header{"Authorization": {"AWS part-one"}}},
		2: {Method: "PUT", URL: "https://objects.example/part-2", Headers: http.Header{"Authorization": {"AWS part-two"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authorization = %#v, want %#v", got, want)
	}
}

func TestRefreshSingleParsesReplacementSignedPUT(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/osuploadrefresh")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"docid":"gns://file","rev":"upload-r1","length":43,"multiupload":false,"reqmethod":"PUT"}` {
			t.Fatalf("body = %s", body)
		}
		_, _ = io.WriteString(w, `{"authrequest":["PUT","https://objects.example/refreshed?signature=synthetic","Authorization: AWS refreshed"]}`)
	}))
	defer server.Close()

	got, err := newTestClient(server, staticToken("fixture-access")).RefreshUpload(context.Background(), "gns://file", "upload-r1", 43, false)
	if err != nil {
		t.Fatal(err)
	}
	want := &anyshare.SignedRequest{Method: "PUT", URL: "https://objects.example/refreshed?signature=synthetic", Headers: http.Header{"Authorization": {"AWS refreshed"}}}
	if !reflect.DeepEqual(got.Request, want) || got.UploadID != "" {
		t.Fatalf("refresh = %#v, want request %#v", got, want)
	}
}

func TestCompleteMultipartParsesDeployedRawBoundaryResponse(t *testing.T) {
	for _, contentType := range []string{"multipart/form-data; boundary=ResponseBoundary123", ""} {
		t.Run(fmt.Sprintf("content-type-%t", contentType != ""), func(t *testing.T) {
			const completionXML = `<CompleteMultipartUpload><Part><ETag>fixture</ETag></Part></CompleteMultipartUpload>`
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/oscompleteupload")
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				}
				_, _ = io.WriteString(w, "--ResponseBoundary123\r\n"+completionXML+"\r\n--ResponseBoundary123\r\n"+
					`{"authrequest":["POST","https://objects.example/complete","Authorization: AWS complete","x-as-userid: remove-me"]}`+"\r\n--ResponseBoundary123--\r\n")
			}))
			defer server.Close()

			request, body, err := newTestClient(server, staticToken("fixture-access")).CompleteMultipart(context.Background(), "gns://file", "upload-r1", "multipart-1", map[int]anyshare.PartInfo{1: {ETag: `"etag"`, Size: 17}})
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != completionXML || request.Method != http.MethodPost || request.URL != "https://objects.example/complete" || request.Headers.Get("Authorization") != "AWS complete" || request.Headers.Get("x-as-userid") != "" {
				t.Fatalf("request=%#v body=%q", request, body)
			}
		})
	}
}

func TestSignedArrayRejectsUnsafeOrMalformedValuesWithoutLeakingThem(t *testing.T) {
	tests := []struct {
		name string
		auth string
	}{
		{name: "plaintext URL", auth: `["PUT","http://objects.example/item?signature=plaintext-marker","Authorization: AWS fixture"]`},
		{name: "unsupported method", auth: `["POST","https://objects.example/item?signature=method-marker","Authorization: AWS fixture"]`},
		{name: "malformed header", auth: `["PUT","https://objects.example/item?signature=header-marker","Authorization AWS malformed-secret"]`},
		{name: "leading header whitespace", auth: `["PUT","https://objects.example/item?signature=header-marker"," X-Signed: malformed-secret"]`},
		{name: "trailing header whitespace", auth: `["PUT","https://objects.example/item?signature=header-marker","X-Signed : malformed-secret"]`},
		{name: "header control byte", auth: `["PUT","https://objects.example/item?signature=header-marker","X-Signed: malformed-\u0001-secret"]`},
		{name: "OAuth bearer", auth: `["PUT","https://objects.example/item?signature=bearer-marker","Authorization: Bearer oauth-secret-marker"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"authrequest":`+tt.auth+`,"docid":"gns://file","rev":"upload-r1"}`)
			}))
			defer server.Close()

			_, err := newTestClient(server, staticToken("fixture-access")).BeginSingle(context.Background(), anyshare.BeginRequest{ParentID: "gns://parent", Name: "new.bin", Size: 17})
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
				t.Fatalf("err=%v, want remote category", err)
			}
			for _, marker := range []string{"objects.example", "signature=", "-marker", "oauth-secret", "malformed-secret"} {
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("error leaked signed request marker %q: %v", marker, err)
				}
			}
		})
	}
}

func TestSignedRequestIsRedactedFromSerializationAndFormatting(t *testing.T) {
	signed := anyshare.SignedRequest{
		Method: "PUT",
		URL:    "https://objects.example/path-marker?signature=query-marker",
		Headers: http.Header{
			"Authorization": {"AWS authorization-marker"},
			"X-Amz-Token":   {"header-marker"},
		},
	}
	encoded, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range []string{string(encoded), fmt.Sprint(signed), fmt.Sprintf("%+v", signed), fmt.Sprintf("%#v", signed)} {
		for _, marker := range []string{"objects.example", "path-marker", "query-marker", "authorization-marker", "header-marker"} {
			if strings.Contains(diagnostic, marker) {
				t.Fatalf("diagnostic %q leaked %q", diagnostic, marker)
			}
		}
	}
}

func TestPutSignedUsesOnlySignedHeadersExactLengthAndGetBody(t *testing.T) {
	const payload = "signed request payload"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPut || r.RequestURI != "/object?signature=synthetic" {
			t.Fatalf("request = %s %s", r.Method, r.RequestURI)
		}
		if r.Header.Get("Authorization") != "AWS object-signature" || r.Header.Get("X-Signed") != "fixture" {
			t.Fatalf("headers = %#v", r.Header)
		}
		if r.Header.Get("User-Agent") != "" || r.Header.Get("Accept-Encoding") != "" {
			t.Fatalf("unsigned default headers reached object store: %#v", r.Header)
		}
		if r.Host != "signed-host.example" {
			t.Fatalf("Host = %q, want signed override", r.Host)
		}
		if strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
			t.Fatalf("OAuth bearer reached object store: %#v", r.Header)
		}
		if r.ContentLength != int64(len(payload)) {
			t.Fatalf("content length = %d, want %d", r.ContentLength, len(payload))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != payload {
			t.Fatalf("body = %q", body)
		}
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header()["eTaG"] = []string{`"synthetic-etag"`}
	}))
	defer server.Close()

	tokens := &tokenSourceFake{}
	transport := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 2})
	transport.Sleep = func(context.Context, time.Duration) error { return nil }
	client := anyshare.NewClient(server.URL, transport, tokens)
	getBodyCalls := 0
	etag, err := client.PutSigned(context.Background(), anyshare.SignedRequest{
		Method: http.MethodPut,
		URL:    server.URL + "/object?signature=synthetic",
		Headers: http.Header{
			"Authorization":  {"AWS object-signature"},
			"Content-Length": {"999"},
			"Host":           {"signed-host.example"},
			"X-Signed":       {"fixture"},
		},
	}, strings.NewReader(payload), func() (io.ReadCloser, error) {
		getBodyCalls++
		return io.NopCloser(strings.NewReader(payload)), nil
	}, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if etag != `"synthetic-etag"` || requests != 2 || getBodyCalls != 1 {
		t.Fatalf("etag=%q requests=%d getBodyCalls=%d", etag, requests, getBodyCalls)
	}
	if len(tokens.calls()) != 0 {
		t.Fatalf("OAuth token calls=%v, want none", tokens.calls())
	}
}

func TestPutSignedObjectPinAcceptsOnlyExactHostnameValidSPKI(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPut {
			t.Fatalf("method=%s", r.Method)
		}
		w.Header().Set("ETag", `"pinned-etag"`)
	}))
	defer server.Close()

	digest := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", hex.EncodeToString(digest[:]))
	client := anyshare.NewClient("https://control.example", httpx.New(nil, httpx.Policy{MaxAttempts: 1}), &tokenSourceFake{})
	etag, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if etag != `"pinned-etag"` || requests != 1 {
		t.Fatalf("etag=%q requests=%d", etag, requests)
	}

	t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", strings.Repeat("0", 64))
	_, err = client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Network || requests != 1 {
		t.Fatalf("wrong-pin err=%v requests=%d, want network failure before HTTP", err, requests)
	}

	t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", "invalid-pin-marker")
	_, err = client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
	if !errors.As(err, &appErr) || appErr.Category != apperr.Local || requests != 1 {
		t.Fatalf("invalid-pin err=%v requests=%d, want local failure before HTTP", err, requests)
	}
	if strings.Contains(err.Error(), "invalid-pin-marker") {
		t.Fatalf("error leaked pin marker: %v", err)
	}
}

func TestPutSignedLoadsPKUObjectPinFromPersistentConfig(t *testing.T) {
	tests := []struct {
		name       string
		configRoot func(*testing.T, string) string
	}{
		{
			name: "XDG config home",
			configRoot: func(t *testing.T, root string) string {
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
				return filepath.Join(root, "xdg-config")
			},
		},
		{
			name: "XDG config home trailing slash",
			configRoot: func(t *testing.T, root string) string {
				clean := filepath.Join(root, "xdg-config")
				t.Setenv("XDG_CONFIG_HOME", clean+string(filepath.Separator))
				return clean
			},
		},
		{
			name: "XDG config home dot component",
			configRoot: func(t *testing.T, root string) string {
				clean := filepath.Join(root, "xdg-config")
				t.Setenv("XDG_CONFIG_HOME", clean+string(filepath.Separator)+"."+string(filepath.Separator))
				return clean
			},
		},
		{
			name: "HOME fallback",
			configRoot: func(t *testing.T, root string) string {
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv("HOME", filepath.Join(root, "home"))
				return filepath.Join(root, "home", ".config")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", "")
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("ETag", `"persistent-pin-etag"`)
			}))
			defer server.Close()

			digest := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
			writeObjectPinConfig(t, tt.configRoot(t, t.TempDir()), `{"spki_sha256":"`+hex.EncodeToString(digest[:])+`"}`, 0o600)
			client := anyshare.NewClient("https://disk.pku.edu.cn", httpx.New(nil, httpx.Policy{MaxAttempts: 1}), &tokenSourceFake{})
			etag, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if etag != `"persistent-pin-etag"` || requests != 1 {
				t.Fatalf("etag=%q requests=%d", etag, requests)
			}
		})
	}
}

func TestPutSignedObjectPinEnvironmentPrecedesPersistentConfig(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("ETag", `"environment-pin-etag"`)
	}))
	defer server.Close()

	digest := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)
	t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", hex.EncodeToString(digest[:]))
	configHome := filepath.Join(t.TempDir(), "xdg-config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeObjectPinConfig(t, configHome, `{"spki_sha256":"persistent-secret-marker"`, 0o644)

	client := anyshare.NewClient("https://disk.pku.edu.cn", httpx.New(nil, httpx.Policy{MaxAttempts: 1}), &tokenSourceFake{})
	etag, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if etag != `"environment-pin-etag"` || requests != 1 {
		t.Fatalf("etag=%q requests=%d", etag, requests)
	}
}

func TestPutSignedRejectsUnsafePersistentPKUObjectPin(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T, string)
	}{
		{name: "malformed JSON", build: func(t *testing.T, home string) {
			writeObjectPinConfig(t, home, `{"spki_sha256":"config-secret-marker"`, 0o600)
		}},
		{name: "unknown field", build: func(t *testing.T, home string) {
			writeObjectPinConfig(t, home, `{"spki_sha256":"`+strings.Repeat("0", 64)+`","unexpected":true}`, 0o600)
		}},
		{name: "duplicate field", build: func(t *testing.T, home string) {
			writeObjectPinConfig(t, home, `{"spki_sha256":"`+strings.Repeat("0", 64)+`","spki_sha256":"`+strings.Repeat("1", 64)+`"}`, 0o600)
		}},
		{name: "invalid pin", build: func(t *testing.T, home string) {
			writeObjectPinConfig(t, home, `{"spki_sha256":"config-secret-marker"}`, 0o600)
		}},
		{name: "wide mode", build: func(t *testing.T, home string) {
			writeObjectPinConfig(t, home, `{"spki_sha256":"`+strings.Repeat("0", 64)+`"}`, 0o644)
		}},
		{name: "symlink", build: func(t *testing.T, home string) {
			dir := makeObjectPinConfigDir(t, home)
			target := filepath.Join(dir, "target.json")
			if err := os.WriteFile(target, []byte(`{"spki_sha256":"`+strings.Repeat("0", 64)+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "object-pin.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non-regular", build: func(t *testing.T, home string) {
			dir := makeObjectPinConfigDir(t, home)
			if err := os.Mkdir(filepath.Join(dir, "object-pin.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", "")
			configHome := filepath.Join(t.TempDir(), "xdg-config")
			t.Setenv("XDG_CONFIG_HOME", configHome)
			tt.build(t, configHome)

			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			client := anyshare.NewClient("https://disk.pku.edu.cn", httpx.New(nil, httpx.Policy{MaxAttempts: 1}), &tokenSourceFake{})
			_, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Local || requests != 0 {
				t.Fatalf("err=%v requests=%d, want local failure before HTTP", err, requests)
			}
			for _, marker := range []string{"config-secret-marker", strings.Repeat("0", 64), strings.Repeat("1", 64)} {
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("error leaked pin marker: %v", err)
				}
			}
		})
	}
}

func TestPutSignedMissingPersistentPKUObjectPinKeepsStrictCATLS(t *testing.T) {
	t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "missing-xdg-config"))
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()

	client := anyshare.NewClient("https://disk.pku.edu.cn", httpx.New(nil, httpx.Policy{MaxAttempts: 1}), &tokenSourceFake{})
	_, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Network || requests != 0 {
		t.Fatalf("err=%v requests=%d, want strict-CA network failure", err, requests)
	}
}

func TestPutSignedNonPKUClientIgnoresPersistentPKUObjectPin(t *testing.T) {
	t.Setenv("PKU_DRIVE_OBJECT_SPKI_SHA256", "")
	configHome := filepath.Join(t.TempDir(), "xdg-config")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeObjectPinConfig(t, configHome, `{"spki_sha256":"persistent-secret-marker"`, 0o644)

	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("ETag", `"strict-ca-etag"`)
	}))
	defer server.Close()
	client := anyshare.NewClient("https://control.example", httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1}), &tokenSourceFake{})
	etag, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPut, URL: server.URL + "/object"}, http.NoBody, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if etag != `"strict-ca-etag"` || requests != 1 {
		t.Fatalf("etag=%q requests=%d", etag, requests)
	}
}

func writeObjectPinConfig(t *testing.T, configHome, contents string, mode os.FileMode) {
	t.Helper()
	dir := makeObjectPinConfigDir(t, configHome)
	path := filepath.Join(dir, "object-pin.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func makeObjectPinConfigDir(t *testing.T, configHome string) string {
	t.Helper()
	dir := filepath.Join(configHome, "pku-drive-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPutSignedFallsBackToContentMD5(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-MD5", "synthetic-content-md5")
	}))
	defer server.Close()

	etag, err := newTestClient(server, &tokenSourceFake{}).PutSigned(context.Background(), anyshare.SignedRequest{Method: "PUT", URL: server.URL + "/object"}, http.NoBody, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if etag != "synthetic-content-md5" {
		t.Fatalf("etag = %q", etag)
	}
}

func TestPutSignedSendsCompletionPOSTWithoutTransportRetry(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s", r.Method)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	hx := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 3})
	hx.Sleep = func(context.Context, time.Duration) error { return nil }
	client := anyshare.NewClient("https://control.example", hx, &tokenSourceFake{})
	_, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodPost, URL: server.URL + "/complete"}, strings.NewReader("<CompleteMultipartUpload/>"), nil, int64(len("<CompleteMultipartUpload/>")))
	var applicationError *apperr.Error
	if !errors.As(err, &applicationError) || applicationError.Category != apperr.Remote || requests != 1 {
		t.Fatalf("err=%v requests=%d, want one remote POST failure", err, requests)
	}
}

func TestPutSignedRejectsUnsafeInputBeforeCredentialsOrNetwork(t *testing.T) {
	tests := []struct {
		name   string
		signed anyshare.SignedRequest
	}{
		{name: "plaintext", signed: anyshare.SignedRequest{Method: "PUT", URL: "http://objects.example/path?signature=plaintext-secret"}},
		{name: "method", signed: anyshare.SignedRequest{Method: "DELETE", URL: "https://objects.example/path?signature=method-secret"}},
		{name: "bearer", signed: anyshare.SignedRequest{Method: "PUT", URL: "https://objects.example/path?signature=bearer-secret", Headers: http.Header{"Authorization": {"Bearer oauth-secret"}}}},
		{name: "invalid header name", signed: anyshare.SignedRequest{Method: "PUT", URL: "https://objects.example/path?signature=header-secret", Headers: http.Header{"Bad Header": {"value-secret"}}}},
		{name: "invalid header value", signed: anyshare.SignedRequest{Method: "PUT", URL: "https://objects.example/path?signature=header-secret", Headers: http.Header{"X-Signed": {"value-\x7f-secret"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := &tokenSourceFake{}
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			client := newTestClient(server, tokens)
			_, err := client.PutSigned(context.Background(), tt.signed, http.NoBody, nil, 0)
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
				t.Fatalf("err=%v, want local category", err)
			}
			if len(tokens.calls()) != 0 || requests != 0 {
				t.Fatalf("token calls=%v requests=%d, want no external access", tokens.calls(), requests)
			}
			for _, marker := range []string{"objects.example", "signature=", "-secret", "oauth-secret", "value-secret"} {
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("error leaked signed request marker %q: %v", marker, err)
				}
			}
		})
	}
}

func TestPutSignedRejectsCrossHostRedirectWithoutLeakingURLs(t *testing.T) {
	targetRequests := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetRequests++ }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/redirect-target-secret?signature=redirect-query-secret", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := anyshare.NewClient(source.URL, httpx.New(source.Client().Transport, httpx.Policy{MaxAttempts: 1}), &tokenSourceFake{})
	_, err := client.PutSigned(context.Background(), anyshare.SignedRequest{Method: "PUT", URL: source.URL + "/signed-source-secret?signature=source-query-secret", Headers: http.Header{"Authorization": {"AWS redirect-auth-secret"}}}, http.NoBody, nil, 0)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Network {
		t.Fatalf("err=%v, want network category", err)
	}
	if targetRequests != 0 {
		t.Fatalf("cross-host target requests=%d", targetRequests)
	}
	for _, marker := range []string{"signed-source-secret", "source-query-secret", "redirect-target-secret", "redirect-query-secret", "redirect-auth-secret"} {
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error leaked %q: %v", marker, err)
		}
	}
}

func TestPutSignedRejectsRedirectThatWouldRewritePUT(t *testing.T) {
	targetRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targetRequests++
			return
		}
		http.Redirect(w, r, "/target?signature=redirect-query-secret", http.StatusFound)
	}))
	defer server.Close()

	_, err := newTestClient(server, &tokenSourceFake{}).PutSigned(context.Background(), anyshare.SignedRequest{Method: "PUT", URL: server.URL + "/source?signature=source-query-secret", Headers: http.Header{"Authorization": {"AWS redirect-auth-secret"}}}, http.NoBody, nil, 0)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Network {
		t.Fatalf("err=%v, want network redirect rejection", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target requests=%d, want none", targetRequests)
	}
}

func TestPutSignedRejectsAll307And308RedirectsBeforeForwardingSignedRequest(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const payload = "redirect-body-secret"
			targetRequests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/source":
					if r.Method != http.MethodPut || r.RequestURI != "/source?signature=source-query-secret" || r.Host != "signed-host.example" {
						t.Fatalf("initial request = %s %s host=%q", r.Method, r.RequestURI, r.Host)
					}
					if r.Header.Get("Authorization") != "AWS redirect-auth-secret" || r.Header.Get("X-Signed") != "fixture" || r.Header.Get("Referer") != "" || r.Header.Get("User-Agent") != "" || r.Header.Get("Accept-Encoding") != "" {
						t.Fatalf("initial headers = %#v", r.Header)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != payload || r.ContentLength != int64(len(payload)) {
						t.Fatalf("initial body=%q length=%d err=%v", body, r.ContentLength, err)
					}
					w.Header().Set("Location", "/target?signature=redirect-query-secret")
					w.WriteHeader(status)
				case "/target":
					targetRequests++
					t.Fatalf("redirected request received signed material: uri=%s headers=%#v host=%q", r.RequestURI, r.Header, r.Host)
				default:
					t.Fatalf("unexpected path %q", r.URL.Path)
				}
			}))
			defer server.Close()

			getBody := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(payload)), nil }
			_, err := newTestClient(server, &tokenSourceFake{}).PutSigned(context.Background(), anyshare.SignedRequest{
				Method: "PUT", URL: server.URL + "/source?signature=source-query-secret",
				Headers: http.Header{"Authorization": {"AWS redirect-auth-secret"}, "Host": {"signed-host.example"}, "X-Signed": {"fixture"}},
			}, strings.NewReader(payload), getBody, int64(len(payload)))
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Network {
				t.Fatalf("err=%v, want network redirect rejection", err)
			}
			if targetRequests != 0 {
				t.Fatalf("redirect target requests=%d, want none", targetRequests)
			}
			for _, marker := range []string{"source-query-secret", "redirect-query-secret", "redirect-auth-secret", "redirect-body-secret"} {
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("error leaked %q: %v", marker, err)
				}
			}
		})
	}
}

func TestPutSignedClassifiesOnlyExplicitExpirationResponses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantCode    string
		wantExpired bool
	}{
		{name: "AWS expired token", status: 403, body: `<Error><Code>ExpiredToken</Code><Message>synthetic</Message></Error>`, wantCode: "ExpiredToken", wantExpired: true},
		{name: "security token expired JSON", status: 401, body: `{"code":"SecurityTokenExpired","message":"synthetic"}`, wantCode: "SecurityTokenExpired", wantExpired: true},
		{name: "clock skew", status: 400, body: `<Error><Code>RequestTimeTooSkewed</Code><Message>synthetic</Message></Error>`, wantCode: "RequestTimeTooSkewed", wantExpired: true},
		{name: "explicit request expired message", status: 403, body: `<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>`, wantCode: "AccessDenied", wantExpired: true},
		{name: "Qiniu expired token", status: 401, body: `{"error":"expired token"}`, wantExpired: true},
		{name: "signature mismatch is not expiry", status: 403, body: `<Error><Code>SignatureDoesNotMatch</Code><Message>synthetic</Message></Error>`, wantCode: "SignatureDoesNotMatch"},
		{name: "generic forbidden is not expiry", status: 403, body: `<Error><Code>AccessDenied</Code><Message>forbidden body-secret-marker</Message></Error>`, wantCode: "AccessDenied"},
		{name: "expiration code on server failure is not expiry", status: 500, body: `<Error><Code>ExpiredToken</Code><Message>synthetic</Message></Error>`, wantCode: "ExpiredToken"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			_, err := newTestClient(server, &tokenSourceFake{}).PutSigned(context.Background(), anyshare.SignedRequest{Method: "PUT", URL: server.URL + "/object?signature=error-fixture"}, http.NoBody, nil, 0)
			if anyshare.IsExpiredSignature(err) != tt.wantExpired || errors.Is(err, anyshare.ErrExpiredSignature) != tt.wantExpired {
				t.Fatalf("expired=%v errors.Is=%v err=%v", anyshare.IsExpiredSignature(err), errors.Is(err, anyshare.ErrExpiredSignature), err)
			}
			var signedErr *anyshare.SignedError
			if !errors.As(err, &signedErr) || signedErr.StatusCode != tt.status || signedErr.Code != tt.wantCode {
				t.Fatalf("signed error = %#v, want status=%d code=%q", signedErr, tt.status, tt.wantCode)
			}
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
				t.Fatalf("err=%v, want remote category", err)
			}
			if strings.Contains(err.Error(), "body-secret-marker") || strings.Contains(err.Error(), "error-fixture") {
				t.Fatalf("error leaked response or URL: %v", err)
			}
		})
	}
}

func TestPutSignedBoundsObjectStoreErrorParsing(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"expired token"}`+strings.Repeat("x", 70<<10)+"body-tail-secret")
	}))
	defer server.Close()

	_, err := newTestClient(server, &tokenSourceFake{}).PutSigned(context.Background(), anyshare.SignedRequest{Method: "PUT", URL: server.URL + "/object"}, http.NoBody, nil, 0)
	if anyshare.IsExpiredSignature(err) || strings.Contains(err.Error(), "body-tail-secret") {
		t.Fatalf("oversized body affected classification or error text: %v", err)
	}
}
