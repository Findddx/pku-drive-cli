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
	"github.com/Findddx/pku-drive-cli/internal/httpx"
)

func TestAuthorizeDownloadUsesDeployedOSDownloadShape(t *testing.T) {
	objectServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer objectServer.Close()
	signedURL := objectServer.URL + "/object?signature=synthetic-download"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthorizedRequest(t, r, http.MethodPost, "/api/efast/v1/file/osdownload")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"docid":"gns://library/file","authtype":"QUERY_STRING","savename":"report.tsv","usehttps":true,"rev":"revision-7"}`
		if string(body) != want {
			t.Fatalf("body = %s, want %s", body, want)
		}
		_, _ = fmt.Fprintf(w, `{"authrequest":["GET",%q,"X-Signed: fixture-download","x-as-userid: must-not-leave-control-plane"]}`, signedURL)
	}))
	defer server.Close()

	got, err := newTestClient(server, staticToken("fixture-access")).AuthorizeDownload(
		context.Background(), "gns://library/file", "revision-7", "report.tsv",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := anyshare.SignedRequest{Method: http.MethodGet, URL: signedURL, Headers: http.Header{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authorization = %#v, want %#v", got, want)
	}
}

func TestAuthorizeDownloadRejectsInvalidSignedMethodWithoutLeaking(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"authrequest":["PUT","https://objects.example/private?signature=download-secret","Authorization: AWS private-secret"]}`)
	}))
	defer server.Close()

	_, err := newTestClient(server, staticToken("fixture-access")).AuthorizeDownload(context.Background(), "gns://library/file", "r1", "report.tsv")
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Remote {
		t.Fatalf("err = %v, want remote error", err)
	}
	for _, secret := range []string{"objects.example", "download-secret", "private-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
}

func TestGetSignedSendsNoControlPlaneHeadersAndReturnsLength(t *testing.T) {
	const payload = "download payload"
	objectServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.RequestURI != "/object?signature=synthetic-download" {
			t.Fatalf("request = %s %s", r.Method, r.RequestURI)
		}
		if r.Header.Get("X-Signed") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("x-as-userid") != "" {
			t.Fatalf("headers = %#v", r.Header)
		}
		if r.Header.Get("User-Agent") != "" || r.Header.Get("Accept-Encoding") != "" {
			t.Fatalf("unsigned default headers reached object store: %#v", r.Header)
		}
		_, _ = io.WriteString(w, payload)
	}))
	defer objectServer.Close()

	tokens := &tokenSourceFake{}
	client := anyshare.NewClient(objectServer.URL, httpx.New(objectServer.Client().Transport, httpx.Policy{MaxAttempts: 1}), tokens)
	stream, err := client.GetSigned(context.Background(), anyshare.SignedRequest{
		Method: http.MethodGet,
		URL:    objectServer.URL + "/object?signature=synthetic-download",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	body, err := io.ReadAll(stream.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != payload || stream.ContentLength != int64(len(payload)) {
		t.Fatalf("body=%q length=%d", body, stream.ContentLength)
	}
	if len(tokens.calls()) != 0 {
		t.Fatalf("OAuth token reached object request: %v", tokens.calls())
	}
}

func TestGetSignedRejectsRedirect(t *testing.T) {
	targetCalls := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL+"/secret-target", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := anyshare.NewClient(source.URL, httpx.New(source.Client().Transport, httpx.Policy{MaxAttempts: 1}), staticToken("unused"))
	_, err := client.GetSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodGet, URL: source.URL + "/signed?signature=redirect-secret"})
	if err == nil || targetCalls != 0 {
		t.Fatalf("err=%v targetCalls=%d, want rejected redirect", err, targetCalls)
	}
	if strings.Contains(err.Error(), "redirect-secret") || strings.Contains(err.Error(), "secret-target") {
		t.Fatalf("redirect error leaked signed URL: %v", err)
	}
}

func TestGetSignedClassifiesExplicitSignatureExpiration(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>ExpiredToken</Code><Message>synthetic</Message></Error>`)
	}))
	defer server.Close()

	client := anyshare.NewClient(server.URL, httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1}), staticToken("unused"))
	_, err := client.GetSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodGet, URL: server.URL + "/signed?signature=expired-secret"})
	if !anyshare.IsExpiredSignature(err) {
		t.Fatalf("err = %v, want expired signature", err)
	}
	if strings.Contains(err.Error(), "expired-secret") {
		t.Fatalf("error leaked signed URL: %v", err)
	}
}

func TestGetSignedRejectsUnsafeInputBeforeNetwork(t *testing.T) {
	tests := []anyshare.SignedRequest{
		{Method: http.MethodPut, URL: "https://objects.example/method-secret"},
		{Method: http.MethodGet, URL: "http://objects.example/plaintext-secret"},
		{Method: http.MethodGet, URL: "https://objects.example/bearer-secret", Headers: http.Header{"Authorization": {"Bearer oauth-secret"}}},
		{Method: http.MethodGet, URL: "https://objects.example/length-secret", Headers: http.Header{"Content-Length": {"1"}}},
		{Method: http.MethodGet, URL: "https://objects.example/encoding-secret", Headers: http.Header{"Accept-Encoding": {"gzip"}}},
		{Method: http.MethodGet, URL: "https://objects.example/custom-header-secret", Headers: http.Header{"X-Signed": {"must-not-leave-control-plane"}}},
	}
	for _, signed := range tests {
		t.Run(signed.URL, func(t *testing.T) {
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			tokens := &tokenSourceFake{}
			client := newTestClient(server, tokens)
			_, err := client.GetSigned(context.Background(), signed)
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
				t.Fatalf("err = %v, want local", err)
			}
			if requests != 0 || len(tokens.calls()) != 0 {
				t.Fatalf("requests=%d tokenCalls=%v", requests, tokens.calls())
			}
			for _, secret := range []string{"method-secret", "plaintext-secret", "bearer-secret", "oauth-secret", "length-secret", "encoding-secret", "custom-header-secret", "must-not-leave-control-plane"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestGetSignedRejectsEncodedSuccessResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "not-the-raw-object")
	}))
	defer server.Close()
	client := anyshare.NewClient(server.URL, httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1}), staticToken("unused"))
	stream, err := client.GetSigned(context.Background(), anyshare.SignedRequest{Method: http.MethodGet, URL: server.URL + "/signed"})
	if err == nil || stream.Body != nil {
		t.Fatalf("stream=%#v err=%v, want rejection", stream, err)
	}
}
