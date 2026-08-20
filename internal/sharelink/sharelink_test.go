package sharelink_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
	"github.com/Findddx/pku-drive-cli/internal/sharelink"
)

type staticToken string

func (s staticToken) Token(context.Context, bool) (string, error) { return string(s), nil }

func TestAnonymousSessionKeepsLinkTokenPrivateAndResolvesNestedFiles(t *testing.T) {
	const (
		linkID = "AA0123456789ABCDEF"
		secret = "anonymous-link-token-secret"
	)
	var mu sync.Mutex
	var requests []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.RequestURI)
		mu.Unlock()
		switch r.RequestURI {
		case "/link/" + linkID:
			assertNoAuthorization(t, r)
			setLegacyLinkCookie(w, linkID, secret)
			http.Redirect(w, r, "/anyshare/link/"+linkID+"?type=anonymous&item_type=folder&password_required=false&verify_mobile=false&title=Fixture", http.StatusFound)
		case "/anyshare/link/" + linkID + "?type=anonymous&item_type=folder&password_required=false&verify_mobile=false&title=Fixture":
			assertNoAuthorization(t, r)
			assertCookie(t, r, "link_token:"+linkID, secret)
			_, _ = io.WriteString(w, "landing")
		case "/api/shared-link/v1/links/" + linkID:
			assertNoAuthorization(t, r)
			if r.Header.Get("Cookie") != "" {
				t.Fatalf("metadata cookie = %q, want empty", r.Header.Get("Cookie"))
			}
			// PKU's deployed anonymous resolver omits item.id; the exact root
			// identity is returned by /api/efast/v1/entry-item.
			_, _ = io.WriteString(w, `{"type":"anonymous","id":"`+linkID+`","item":{"belongs_to":"document","type":"folder"}}`)
		case "/api/efast/v1/entry-item":
			assertBearer(t, r, secret)
			assertNoCookie(t, r)
			_, _ = io.WriteString(w, `[{"id":"gns://shared-root","name":"Fixture","type":"folder","rev":"root-r1","size":-1}]`)
		case "/api/efast/v1/folders/gns:%2F%2Fshared-root/sub_objects?limit=1000":
			assertBearer(t, r, secret)
			assertNoCookie(t, r)
			_, _ = io.WriteString(w, `{"dirs":[{"id":"gns://nested","name":"nested","type":"folder","size":-1}],"files":[{"id":"gns://z-file","name":"z.tsv","type":"file","rev":"z-r1","size":9}],"next_marker":"page two"}`)
		case "/api/efast/v1/folders/gns:%2F%2Fshared-root/sub_objects?limit=1000&marker=page+two":
			assertBearer(t, r, secret)
			assertNoCookie(t, r)
			_, _ = io.WriteString(w, `{"dirs":[],"files":[{"id":"gns://a-file","name":"a.tsv","type":"file","rev":"a-r1","size":3}],"next_marker":""}`)
		case "/api/efast/v1/folders/gns:%2F%2Fnested/sub_objects?limit=1000":
			assertBearer(t, r, secret)
			assertNoCookie(t, r)
			_, _ = io.WriteString(w, `{"dirs":[],"files":[{"docid":"gns://deep-file","name":"deep.bin","type":"file","rev":"deep-r2","size":21}],"next_marker":""}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	opener := newOpener(t, server, nil)
	session, err := opener.Open(context.Background(), server.URL+"/link/"+linkID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Type() != sharelink.LinkAnonymous {
		t.Fatalf("type = %q", session.Type())
	}
	wantRoot := sharelink.Entry{
		Name: "Fixture", Type: "directory", Size: -1,
		Item: anyshare.Item{ID: "gns://shared-root", Name: "Fixture", Type: "directory", Rev: "root-r1", Size: -1},
	}
	if got := session.Root(); !reflect.DeepEqual(got, wantRoot) {
		t.Fatalf("root = %#v, want %#v", got, wantRoot)
	}
	entries, err := session.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"nested", "a.tsv", "z.tsv"}
	gotPaths := make([]string, len(entries))
	for i := range entries {
		gotPaths[i] = entries[i].Path
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("paths = %q, want %q", gotPaths, wantPaths)
	}
	deep, err := session.Resolve(context.Background(), "nested/deep.bin")
	if err != nil {
		t.Fatal(err)
	}
	if deep.Path != "nested/deep.bin" || deep.Item.ID != "gns://deep-file" || deep.Item.Rev != "deep-r2" {
		t.Fatalf("deep = %#v", deep)
	}
	if strings.Contains(fmt.Sprint(requests), secret) {
		t.Fatalf("request targets leaked token: %v", requests)
	}
	if len(requests) == 0 || requests[0] != "GET /api/shared-link/v1/links/"+linkID {
		t.Fatalf("first request = %q, want unauthenticated metadata resolution", requests)
	}
}

func TestRealnameFileUsesAuthenticatedClient(t *testing.T) {
	const linkID = "AR0123456789ABCDEF"
	var landingRequests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/link/" + linkID:
			landingRequests.Add(1)
			http.Error(w, "real-name landing must not be opened", http.StatusInternalServerError)
		case "/anyshare/link/" + linkID:
			landingRequests.Add(1)
			http.Error(w, "real-name landing must not be opened", http.StatusInternalServerError)
		case "/api/shared-link/v1/links/" + linkID:
			assertNoAuthorization(t, r)
			_, _ = io.WriteString(w, `{"type":"realname","id":"`+linkID+`","item":{"belongs_to":"document","id":"gns://real-file","type":"file"}}`)
		case "/api/efast/v1/file/metadata":
			assertBearer(t, r, "oauth-access")
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"docid":"gns://real-file"}` {
				t.Fatalf("body = %s", body)
			}
			_, _ = io.WriteString(w, `{"docid":"gns://real-file","name":"report.tsv","rev":"real-r1","type":"file","size":41}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	transport := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	authenticated := anyshare.NewClient(server.URL, transport, staticToken("oauth-access"))
	opener, err := sharelink.NewOpener(server.URL, transport, authenticated)
	if err != nil {
		t.Fatal(err)
	}
	session, err := opener.Open(context.Background(), server.URL+"/link/"+linkID)
	if err != nil {
		t.Fatal(err)
	}
	want := sharelink.Entry{
		Path: "report.tsv", Name: "report.tsv", Type: "file", Size: 41,
		Item: anyshare.Item{ID: "gns://real-file", DocID: "gns://real-file", Name: "report.tsv", Type: "file", Rev: "real-r1", Size: 41},
	}
	if got := session.Root(); !reflect.DeepEqual(got, want) {
		t.Fatalf("root = %#v, want %#v", got, want)
	}
	if landingRequests.Load() != 0 {
		t.Fatalf("real-name landing requests = %d, want 0", landingRequests.Load())
	}
	resolved, err := session.Resolve(context.Background(), "report.tsv")
	if err != nil || !reflect.DeepEqual(resolved, want) {
		t.Fatalf("Resolve() = %#v, %v", resolved, err)
	}
}

func TestRealnameLinkClassifiesMissingLogin(t *testing.T) {
	const linkID = "ARLOGINREQUIRED"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/shared-link/v1/links/" + linkID:
			_, _ = io.WriteString(w, `{"type":"realname","id":"`+linkID+`","item":{"belongs_to":"document","id":"gns://private-file","type":"file"}}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	transport := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	authenticated := anyshare.NewClient(server.URL, transport, staticToken(""))
	opener, err := sharelink.NewOpener(server.URL, transport, authenticated)
	if err != nil {
		t.Fatal(err)
	}
	_, err = opener.Open(context.Background(), server.URL+"/link/"+linkID)
	if !errors.Is(err, sharelink.ErrAuthenticationRequired) {
		t.Fatalf("err = %v, want authentication required", err)
	}
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Auth {
		t.Fatalf("err = %v, want auth category", err)
	}
	if strings.Contains(err.Error(), linkID) || strings.Contains(err.Error(), "private-file") {
		t.Fatalf("error leaked link data: %v", err)
	}
}

func TestOpenRejectsInvalidAndCrossOriginLinksWithoutDisclosure(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	opener := newOpener(t, server, nil)
	host := strings.TrimPrefix(server.URL, "https://")
	tests := []string{
		"http://" + host + "/link/plaintextsecret",
		"https://userinfosecret@" + host + "/link/userinfosecret",
		server.URL + "/link/pathsecret/extra",
		server.URL + "/link/encoded%2Fslashsecret",
		server.URL + "/link/querysecret?token=query-secret",
		server.URL + "/link/fragmentsecret#fragment-secret",
		server.URL + "/link/not_opaque",
		"https://example.invalid/link/crossoriginsecret",
	}
	for _, raw := range tests {
		_, err := opener.Open(context.Background(), raw)
		if !errors.Is(err, sharelink.ErrInvalidLink) {
			t.Fatalf("Open(%q) err = %v, want invalid link", raw, err)
		}
		for _, marker := range []string{"secret", host, raw} {
			if marker != "" && strings.Contains(err.Error(), marker) {
				t.Fatalf("error disclosed rejected input: %v", err)
			}
		}
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestOpenRejectsCrossOriginRedirectBeforeSendingCookieOrToken(t *testing.T) {
	const (
		linkID = "AACROSSORIGIN"
		secret = "redirect-cookie-secret"
	)
	evilRequests := 0
	evil := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { evilRequests++ }))
	defer evil.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/shared-link/v1/links/" + linkID:
			assertNoAuthorization(t, r)
			_, _ = io.WriteString(w, `{"type":"anonymous","id":"`+linkID+`","item":{"belongs_to":"document","type":"folder"}}`)
		case "/link/" + linkID:
			setLegacyLinkCookie(w, linkID, secret)
			http.Redirect(w, r, evil.URL+"/steal/"+linkID, http.StatusFound)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	opener := newOpener(t, server, nil)
	_, err := opener.Open(context.Background(), server.URL+"/link/"+linkID)
	if err == nil {
		t.Fatal("Open() error = nil")
	}
	if evilRequests != 0 {
		t.Fatalf("cross-origin requests = %d, want 0", evilRequests)
	}
	if strings.Contains(err.Error(), linkID) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), evil.URL) {
		t.Fatalf("error leaked redirect data: %v", err)
	}
}

func TestOpenStopsBeforeAnonymousAPIForRestrictedOrExpiredLinks(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  error
	}{
		{name: "expired", query: "expires_at=2000-01-01T00%3A00%3A00Z", want: sharelink.ErrExpired},
		{name: "password", query: "password_required=true", want: sharelink.ErrPasswordRequired},
		{name: "mobile", query: "verify_mobile=true", want: sharelink.ErrMobileVerificationRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const linkID = "AARESTRICTED"
			entryRequests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.EscapedPath() {
				case "/link/" + linkID:
					setLegacyLinkCookie(w, linkID, "ephemeral-secret")
					http.Redirect(w, r, "/anyshare/link/"+linkID+"?type=anonymous&item_type=folder&"+tt.query, http.StatusFound)
				case "/anyshare/link/" + linkID:
					_, _ = io.WriteString(w, "landing")
				case "/api/shared-link/v1/links/" + linkID:
					_, _ = io.WriteString(w, `{"type":"anonymous","id":"`+linkID+`","item":{"belongs_to":"document","id":"gns://root","type":"folder"}}`)
				case "/api/efast/v1/entry-item":
					entryRequests++
				default:
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			defer server.Close()

			_, err := newOpener(t, server, nil).Open(context.Background(), server.URL+"/link/"+linkID)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if entryRequests != 0 {
				t.Fatalf("entry requests = %d, want 0", entryRequests)
			}
		})
	}
}

func TestSessionRejectsUnsafeRelativePathsBeforeNetworkAccess(t *testing.T) {
	session, server, listRequests := openFolderFixture(t)
	defer server.Close()
	for _, path := range []string{"/absolute", "trailing/", "double//slash", "./file", "dir/../file", `dir\file`, "nul\x00file"} {
		_, err := session.Resolve(context.Background(), path)
		if err == nil {
			t.Fatalf("Resolve(%q) error = nil", path)
		}
		var appErr *apperr.Error
		if !errors.As(err, &appErr) || appErr.Category != apperr.Usage {
			t.Fatalf("Resolve(%q) err = %v, want usage", path, err)
		}
	}
	if *listRequests != 0 {
		t.Fatalf("list requests = %d, want 0", *listRequests)
	}
}

func TestDownloadPreflightsEveryDestinationBeforeTransfer(t *testing.T) {
	const linkID = "ARPRECHECKBATCH"
	metadataRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/link/" + linkID:
			http.Redirect(w, r, "/anyshare/link/"+linkID+"?type=realname&item_type=folder", http.StatusFound)
		case "/anyshare/link/" + linkID:
			_, _ = io.WriteString(w, "landing")
		case "/api/shared-link/v1/links/" + linkID:
			_, _ = io.WriteString(w, `{"type":"realname","id":"`+linkID+`","item":{"belongs_to":"document","id":"gns://root","type":"folder"}}`)
		case "/api/efast/v1/folders/gns:%2F%2Froot/sub_objects":
			_, _ = io.WriteString(w, `{"dirs":[],"files":[{"id":"gns://a","name":"a.bin","type":"file","rev":"a-r1","size":1},{"id":"gns://b","name":"b.bin","type":"file","rev":"b-r1","size":1}],"next_marker":""}`)
		case "/api/efast/v1/file/metadata":
			metadataRequests++
			http.Error(w, "must not transfer", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	transport := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	authenticated := anyshare.NewClient(server.URL, transport, staticToken("oauth-access"))
	opener, err := sharelink.NewOpener(server.URL, transport, authenticated)
	if err != nil {
		t.Fatal(err)
	}
	session, err := opener.Open(context.Background(), server.URL+"/link/"+linkID)
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "b.bin"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = session.Download(context.Background(), []string{"a.bin", "b.bin"}, destination, false, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != apperr.Local {
		t.Fatalf("err = %v, want local preflight error", err)
	}
	if metadataRequests != 0 {
		t.Fatalf("metadata requests = %d, want 0", metadataRequests)
	}
	if _, err := os.Stat(filepath.Join(destination, "a.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a.bin err = %v, want not exist", err)
	}
}

func TestDownloadPreservesRelativeLayoutOrderAndAggregateProgress(t *testing.T) {
	const linkID = "ARDOWNLOADBATCH"
	payloads := map[string]string{"gns://a": "AAA", "gns://b": "BBBBB"}
	var failB atomic.Bool
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/link/" + linkID:
			assertNoAuthorization(t, r)
			http.Redirect(w, r, "/anyshare/link/"+linkID+"?type=realname&item_type=folder&title=Batch", http.StatusFound)
		case "/anyshare/link/" + linkID:
			assertNoAuthorization(t, r)
			_, _ = io.WriteString(w, "landing")
		case "/api/shared-link/v1/links/" + linkID:
			assertNoAuthorization(t, r)
			_, _ = io.WriteString(w, `{"type":"realname","id":"`+linkID+`","item":{"belongs_to":"document","id":"gns://root","type":"folder"}}`)
		case "/api/efast/v1/folders/gns:%2F%2Froot/sub_objects":
			assertBearer(t, r, "oauth-access")
			_, _ = io.WriteString(w, `{"dirs":[{"id":"gns://nested","name":"nested","type":"folder","size":-1}],"files":[{"id":"gns://a","name":"a.bin","type":"file","rev":"a-r1","size":3}],"next_marker":""}`)
		case "/api/efast/v1/folders/gns:%2F%2Fnested/sub_objects":
			assertBearer(t, r, "oauth-access")
			_, _ = io.WriteString(w, `{"dirs":[],"files":[{"id":"gns://b","name":"b.bin","type":"file","rev":"b-r1","size":5}],"next_marker":""}`)
		case "/api/efast/v1/file/metadata":
			assertBearer(t, r, "oauth-access")
			body, _ := io.ReadAll(r.Body)
			var id, name, rev string
			switch string(body) {
			case `{"docid":"gns://a","rev":"a-r1"}`:
				id, name, rev = "gns://a", "a.bin", "a-r1"
			case `{"docid":"gns://b","rev":"b-r1"}`:
				id, name, rev = "gns://b", "b.bin", "b-r1"
			default:
				t.Fatalf("metadata body = %s", body)
			}
			_, _ = fmt.Fprintf(w, `{"docid":%q,"name":%q,"type":"file","rev":%q,"size":%d}`, id, name, rev, len(payloads[id]))
		case "/api/efast/v1/file/osdownload":
			assertBearer(t, r, "oauth-access")
			body, _ := io.ReadAll(r.Body)
			id := "gns://a"
			object := "a"
			if strings.Contains(string(body), `"docid":"gns://b"`) {
				id, object = "gns://b", "b"
			}
			_ = id
			_, _ = fmt.Fprintf(w, `{"authrequest":["GET",%q]}`, server.URL+"/objects/"+object+"?signature=fixture")
		case "/objects/a":
			assertNoAuthorization(t, r)
			_, _ = io.WriteString(w, payloads["gns://a"])
		case "/objects/b":
			assertNoAuthorization(t, r)
			if failB.Load() {
				http.Error(w, "fixture failure", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, payloads["gns://b"])
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	server.StartTLS()
	defer server.Close()

	transport := httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1})
	authenticated := anyshare.NewClient(server.URL, transport, staticToken("oauth-access"))
	opener, err := sharelink.NewOpener(server.URL, transport, authenticated)
	if err != nil {
		t.Fatal(err)
	}
	session, err := opener.Open(context.Background(), server.URL+"/link/"+linkID)
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	progress := &progressRecorder{}
	results, err := session.Download(context.Background(), []string{"nested/b.bin", "a.bin"}, destination, false, progress)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Entry.Path != "nested/b.bin" || results[1].Entry.Path != "a.bin" {
		t.Fatalf("results = %#v", results)
	}
	for relative, want := range map[string]string{"nested/b.bin": "BBBBB", "a.bin": "AAA"} {
		got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(relative)))
		if err != nil || string(got) != want {
			t.Fatalf("read %s = %q, %v", relative, got, err)
		}
	}
	if !reflect.DeepEqual(progress.started, []int64{8}) || progress.advanced != 8 || progress.finished != 1 {
		t.Fatalf("progress = %#v", progress)
	}

	failB.Store(true)
	partialDestination := t.TempDir()
	partial, err := session.Download(context.Background(), []string{"a.bin", "nested/b.bin"}, partialDestination, false, nil)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || !strings.Contains(appErr.Message, "after 1 completed file") {
		t.Fatalf("partial error = %v", err)
	}
	if len(partial) != 1 || partial[0].Entry.Path != "a.bin" {
		t.Fatalf("partial results = %#v", partial)
	}
	if got, readErr := os.ReadFile(filepath.Join(partialDestination, "a.bin")); readErr != nil || string(got) != "AAA" {
		t.Fatalf("completed file = %q, %v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(partialDestination, "nested", "b.bin")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed file stat = %v, want not exist", statErr)
	}
}

type progressRecorder struct {
	started  []int64
	advanced int64
	finished int
}

func (p *progressRecorder) Started(total int64)  { p.started = append(p.started, total) }
func (p *progressRecorder) Advanced(delta int64) { p.advanced += delta }
func (p *progressRecorder) Finished()            { p.finished++ }

func openFolderFixture(t *testing.T) (*sharelink.Session, *httptest.Server, *int) {
	t.Helper()
	const linkID = "AAFOLDERFIXTURE"
	listRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/link/" + linkID:
			setLegacyLinkCookie(w, linkID, "fixture-secret")
			http.Redirect(w, r, "/anyshare/link/"+linkID+"?type=anonymous&item_type=folder", http.StatusFound)
		case "/anyshare/link/" + linkID:
			_, _ = io.WriteString(w, "landing")
		case "/api/shared-link/v1/links/" + linkID:
			_, _ = io.WriteString(w, `{"type":"anonymous","id":"`+linkID+`","item":{"belongs_to":"document","id":"gns://root","type":"folder"}}`)
		case "/api/efast/v1/entry-item":
			_, _ = io.WriteString(w, `[{"id":"gns://root","name":"Root","type":"folder","size":-1}]`)
		case "/api/efast/v1/folders/gns:%2F%2Froot/sub_objects":
			listRequests++
			_, _ = io.WriteString(w, `{"dirs":[],"files":[],"next_marker":""}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	session, err := newOpener(t, server, nil).Open(context.Background(), server.URL+"/link/"+linkID)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return session, server, &listRequests
}

func newOpener(t *testing.T, server *httptest.Server, authenticated *anyshare.Client) *sharelink.Opener {
	t.Helper()
	opener, err := sharelink.NewOpener(server.URL, httpx.New(server.Client().Transport, httpx.Policy{MaxAttempts: 1}), authenticated)
	if err != nil {
		t.Fatal(err)
	}
	return opener
}

func assertNoAuthorization(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "" {
		t.Fatalf("authorization = %q, want empty", got)
	}
}

func assertBearer(t *testing.T, r *http.Request, token string) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("authorization = %q", got)
	}
	if strings.Contains(r.RequestURI, token) {
		t.Fatalf("request URI leaked token: %s", r.RequestURI)
	}
}

func assertCookie(t *testing.T, r *http.Request, name, value string) {
	t.Helper()
	want := name + "=" + value
	found := false
	for _, header := range r.Header.Values("Cookie") {
		for _, pair := range strings.Split(header, ";") {
			if strings.TrimSpace(pair) == want {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("cookie %q is missing", name)
	}
}

func assertNoCookie(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Cookie"); got != "" {
		t.Fatalf("cookie = %q, want empty", got)
	}
}

func setLegacyLinkCookie(w http.ResponseWriter, linkID, value string) {
	w.Header().Add("Set-Cookie", "link_token:"+linkID+"="+value+"; Path=/; Secure; HttpOnly")
}
