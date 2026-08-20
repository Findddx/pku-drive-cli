package integration

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/auth"
	"github.com/Findddx/pku-drive-cli/internal/cli"
	"github.com/Findddx/pku-drive-cli/internal/config"
	"github.com/Findddx/pku-drive-cli/internal/download"
	"github.com/Findddx/pku-drive-cli/internal/httpx"
	"github.com/Findddx/pku-drive-cli/internal/oauth"
	"github.com/Findddx/pku-drive-cli/internal/remote"
	"github.com/Findddx/pku-drive-cli/internal/sharelink"
	"github.com/Findddx/pku-drive-cli/internal/upload"
	"github.com/Findddx/pku-drive-cli/internal/version"
)

const (
	fixtureAccess          = "fixture-access"
	fixtureRefreshedAccess = "fixture-access-refreshed"
	fixtureRefresh         = "fixture-refresh"
	fixtureSecret          = "fixture-secret"
	fixtureShareToken      = "fake-share-token-marker"

	fixturePublicLinkID   = "FakePublicLinkMarker"
	fixtureRealnameLinkID = "FakeRealnameLinkMarker"
	fixturePasswordLinkID = "FakePasswordLinkMarker"
	fixtureMobileLinkID   = "FakeMobileLinkMarker"
	fixtureRedirectLinkID = "FakeRedirectLinkMarker"

	fixturePublicRootID = "gns://fixture-share-public-root"
	fixturePublicDirID  = "gns://fixture-share-public-dir"
	fixturePublicFileID = "gns://fixture-share-public-file"
	fixturePublicTopID  = "gns://fixture-share-public-top"
	fixtureRealRootID   = "gns://fixture-share-real-root"
	fixtureRealFileID   = "gns://fixture-share-real-file"
)

var fixtureNow = time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)

type fixture struct {
	t               *testing.T
	mu              sync.Mutex
	control         *httptest.Server
	objects         *httptest.Server
	parts           map[int][]byte
	calls           map[string]int
	instant         map[int64][]byte
	expectedByName  map[string]expectedContent
	expectedBySlice map[string]expectedContent
	puts            map[string]int
	files           map[string][]byte
	nodes           map[string]*fixtureNode
	pending         map[string]*pendingUpload
	faults          map[string]int
	partOK          map[int]bool
	partTry         map[int]int
	gateTwo         bool
	twoOnce         sync.Once
	twoDone         chan struct{}
	redirectURI     string
	activeAccess    string
	seq             int
}

type fixtureNode struct {
	ID, ParentID, Name, Type, Rev, Path string
	Size, ClientMtimeUS                 int64
	MD5, SliceMD5, CRC32                string
}

type pendingUpload struct {
	DocID, ParentID, Name, Rev, UploadID string
	Size, ClientMtimeUS                  int64
	Data                                 []byte
	Parts                                map[int][]byte
	ETags                                map[int]string
	MD5, SliceMD5, CRC32                 string
	SingleGeneration                     int
	CompletionXML                        []byte
}

type expectedContent struct {
	Name, MD5, SliceMD5, CRC32 string
	Data                       []byte
	Size, ClientMtimeUS        int64
	Instant                    bool
}

type completionDocument struct {
	XMLName  xml.Name         `xml:"CompleteMultipartUpload"`
	UploadID string           `xml:"UploadId"`
	Parts    []completionPart `xml:"Part"`
}

type completionPart struct {
	Number int    `xml:"PartNumber"`
	ETag   string `xml:"ETag"`
	Size   int64  `xml:"Size"`
}

type cliOutcome struct {
	code   int
	stdout string
	stderr string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t: t, parts: map[int][]byte{}, calls: map[string]int{}, instant: map[int64][]byte{},
		expectedByName: map[string]expectedContent{}, expectedBySlice: map[string]expectedContent{},
		puts: map[string]int{}, files: map[string][]byte{}, nodes: map[string]*fixtureNode{}, pending: map[string]*pendingUpload{},
		faults: map[string]int{}, partOK: map[int]bool{}, partTry: map[int]int{}, twoDone: make(chan struct{}),
		activeAccess: fixtureAccess,
	}
	f.nodes["gns://library"] = &fixtureNode{ID: "gns://library", Name: "个人文档", Type: "directory", Rev: "library-r1", Path: "/个人文档", Size: -1}
	f.addShareFixtures()
	f.objects = httptest.NewTLSServer(http.HandlerFunc(f.handleObject))
	f.control = httptest.NewTLSServer(http.HandlerFunc(f.handleControl))
	t.Cleanup(f.control.Close)
	t.Cleanup(f.objects.Close)
	return f
}

func (f *fixture) addShareFixtures() {
	addFile := func(id, parentID, name, path, revision string, data []byte) {
		evidence := contentEvidence(name, data, fixtureNow.UnixNano()/1000, false)
		f.nodes[id] = &fixtureNode{
			ID: id, ParentID: parentID, Name: name, Type: "file", Rev: revision, Path: path,
			Size: evidence.Size, ClientMtimeUS: evidence.ClientMtimeUS, MD5: evidence.MD5,
			SliceMD5: evidence.SliceMD5, CRC32: evidence.CRC32,
		}
		f.files[path] = append([]byte(nil), data...)
	}

	f.nodes[fixturePublicRootID] = &fixtureNode{ID: fixturePublicRootID, Name: "公开分享", Type: "directory", Rev: "share-public-root-r1", Path: "/fixture/share/public", Size: -1}
	f.nodes[fixturePublicDirID] = &fixtureNode{ID: fixturePublicDirID, ParentID: fixturePublicRootID, Name: "sub", Type: "directory", Rev: "share-public-dir-r1", Path: "/fixture/share/public/sub", Size: -1}
	addFile(fixturePublicFileID, fixturePublicDirID, "nested.txt", "/fixture/share/public/sub/nested.txt", "share-public-file-r1", []byte("fake anonymous share payload\n"))
	addFile(fixturePublicTopID, fixturePublicRootID, "top.txt", "/fixture/share/public/top.txt", "share-public-top-r1", []byte("fake top-level share payload\n"))

	f.nodes[fixtureRealRootID] = &fixtureNode{ID: fixtureRealRootID, Name: "组织分享", Type: "directory", Rev: "share-real-root-r1", Path: "/fixture/share/real", Size: -1}
	addFile(fixtureRealFileID, fixtureRealRootID, "member.txt", "/fixture/share/real/member.txt", "share-real-file-r1", []byte("fake organization share payload\n"))
}

type fixtureShareInfo struct {
	linkType, itemType, title, rootID string
	passwordRequired, verifyMobile    bool
}

func lookupFixtureShare(linkID string) (fixtureShareInfo, bool) {
	switch linkID {
	case fixturePublicLinkID:
		return fixtureShareInfo{linkType: "anonymous", itemType: "folder", title: "公开分享", rootID: fixturePublicRootID}, true
	case fixtureRealnameLinkID:
		return fixtureShareInfo{linkType: "realname", itemType: "folder", title: "组织分享", rootID: fixtureRealRootID}, true
	case fixturePasswordLinkID:
		return fixtureShareInfo{linkType: "anonymous", itemType: "folder", title: "口令分享", rootID: fixturePublicRootID, passwordRequired: true}, true
	case fixtureMobileLinkID:
		return fixtureShareInfo{linkType: "anonymous", itemType: "folder", title: "手机验证分享", rootID: fixturePublicRootID, verifyMobile: true}, true
	case fixtureRedirectLinkID:
		return fixtureShareInfo{linkType: "anonymous", itemType: "folder", title: "重定向分享", rootID: fixturePublicRootID}, true
	default:
		return fixtureShareInfo{}, false
	}
}

func (f *fixture) startShareLanding(w http.ResponseWriter, r *http.Request) {
	linkID := strings.TrimPrefix(r.URL.Path, "/link/")
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		f.t.Errorf("initial shared-link request carried credentials")
		http.Error(w, "unexpected credentials", http.StatusUnauthorized)
		return
	}
	if linkID == fixtureRedirectLinkID {
		w.Header().Set("Location", f.objects.URL+"/redirect-target")
		w.WriteHeader(http.StatusFound)
		return
	}
	info, ok := lookupFixtureShare(linkID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The legacy AnyShare cookie name contains a colon, so write the fixture
	// header exactly as the server does instead of using http.SetCookie.
	w.Header().Add("Set-Cookie", "link_token:"+linkID+"="+fixtureShareToken+"; Path=/; Max-Age=3600; Secure; HttpOnly; SameSite=Lax")
	query := url.Values{
		"type":              {info.linkType},
		"item_type":         {info.itemType},
		"title":             {info.title},
		"password_required": {strconv.FormatBool(info.passwordRequired)},
		"verify_mobile":     {strconv.FormatBool(info.verifyMobile)},
	}
	w.Header().Set("Location", "/anyshare/link/"+linkID+"?"+query.Encode())
	w.WriteHeader(http.StatusFound)
}

func (f *fixture) finishShareLanding(w http.ResponseWriter, r *http.Request) {
	linkID := strings.TrimPrefix(r.URL.Path, "/anyshare/link/")
	if _, ok := lookupFixtureShare(linkID); !ok {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "" || !hasFixtureShareCookie(r, linkID) {
		f.t.Errorf("shared-link landing did not use its isolated cookie session")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!doctype html><title>fixture share</title>")
}

func (f *fixture) shareMetadata(w http.ResponseWriter, r *http.Request) {
	linkID := strings.TrimPrefix(r.URL.Path, "/api/shared-link/v1/links/")
	info, ok := lookupFixtureShare(linkID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		f.t.Errorf("shared-link metadata request carried credentials: authorization-present=%v cookie-present=%v", r.Header.Get("Authorization") != "", r.Header.Get("Cookie") != "")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{
		"type": info.linkType, "id": linkID, "title": info.title, "expires_at": 0,
		"password_required": info.passwordRequired, "verify_mobile": info.verifyMobile,
		"item": map[string]any{
			"belongs_to": "document", "id": info.rootID, "type": info.itemType, "name": info.title,
		},
	})
}

func hasFixtureShareCookie(r *http.Request, linkID string) bool {
	want := "link_token:" + linkID + "=" + fixtureShareToken
	for _, part := range strings.Split(r.Header.Get("Cookie"), ";") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

func (f *fixture) handleControl(w http.ResponseWriter, r *http.Request) {
	f.recordCall(r.URL.Path)
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/link/"):
		f.startShareLanding(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/anyshare/link/"):
		f.finishShareLanding(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/shared-link/v1/links/"):
		f.shareMetadata(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/oauth2/clients":
		var request struct {
			ClientName             string   `json:"client_name"`
			GrantTypes             []string `json:"grant_types"`
			ResponseTypes          []string `json:"response_types"`
			Scope                  string   `json:"scope"`
			RedirectURIs           []string `json:"redirect_uris"`
			PostLogoutRedirectURIs []string `json:"post_logout_redirect_uris"`
			Metadata               struct {
				Device struct {
					Name        string `json:"name"`
					ClientType  string `json:"client_type"`
					Description string `json:"description"`
				} `json:"device"`
			} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			f.t.Errorf("decode registration: %v", err)
			http.Error(w, "bad registration", http.StatusBadRequest)
			return
		}
		if request.ClientName != "pku-drive-cli" || request.Scope != "offline openid all" || len(request.GrantTypes) != 3 || request.GrantTypes[0] != "authorization_code" || request.GrantTypes[1] != "refresh_token" || request.GrantTypes[2] != "implicit" || len(request.ResponseTypes) != 3 || request.ResponseTypes[0] != "token id_token" || request.ResponseTypes[1] != "code" || request.ResponseTypes[2] != "token" || len(request.RedirectURIs) != 1 || !strings.HasPrefix(request.RedirectURIs[0], "http://127.0.0.1:") || len(request.PostLogoutRedirectURIs) != 1 || request.PostLogoutRedirectURIs[0] != request.RedirectURIs[0] || request.Metadata.Device.Name != "pku-drive-cli" || request.Metadata.Device.ClientType != "web" || request.Metadata.Device.Description != "PKU Drive CLI on Linux" {
			f.t.Errorf("registration request=%+v", request)
			http.Error(w, "bad registration shape", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.redirectURI = request.RedirectURIs[0]
		f.mu.Unlock()
		writeJSON(w, map[string]any{"client_id": "fixture-client", "client_secret": fixtureSecret})
	case r.Method == http.MethodPost && r.URL.Path == "/oauth2/token":
		clientID, secret, ok := r.BasicAuth()
		if !ok || clientID != "fixture-client" || secret != fixtureSecret {
			f.t.Errorf("token basic auth id=%q secret-match=%v", clientID, secret == fixtureSecret)
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			f.t.Errorf("parse token form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.mu.Lock()
			registeredRedirect := f.redirectURI
			f.mu.Unlock()
			if r.Form.Get("code") != "fixture-code" || r.Form.Get("redirect_uri") != registeredRedirect || r.Form.Get("code_verifier") == "" {
				f.t.Errorf("exchange form=%v", r.Form)
				http.Error(w, "bad exchange", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.activeAccess = fixtureAccess
			f.mu.Unlock()
		case "refresh_token":
			if r.Form.Get("refresh_token") != fixtureRefresh {
				f.t.Errorf("refresh form=%v", r.Form)
				http.Error(w, "bad refresh", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.activeAccess = fixtureRefreshedAccess
			f.mu.Unlock()
		default:
			f.t.Errorf("unexpected grant form=%v", r.Form)
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		accessToken := f.activeAccess
		f.mu.Unlock()
		writeJSON(w, map[string]any{"access_token": accessToken, "refresh_token": fixtureRefresh, "token_type": "Bearer", "expires_in": 3600})
	case r.Method == http.MethodPost && r.URL.Path == "/api/eacp/v1/user/get":
		if !f.requireBearer(w, r) {
			return
		}
		writeJSON(w, map[string]any{"userid": "gns://fixture-user", "name": "Fixture User", "account": "fixture-user", "type": "user"})
	case r.Method == http.MethodGet && r.URL.Path == "/api/efast/v1/entry-doc-lib":
		if f.consumeFault("entry-401") {
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}
		if f.consumeFault("entry-429") {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if f.consumeFault("entry-503") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !f.requireBearer(w, r) {
			return
		}
		if r.URL.Query().Get("direction") != "asc" || r.URL.Query().Get("sort") != "doc_lib_name" {
			f.t.Errorf("entry-doc-lib query=%q", r.URL.RawQuery)
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		writeJSON(w, []map[string]any{{"id": "gns://library", "docid": "gns://library", "name": "个人文档", "path": "/个人文档", "type": "user_doc_lib", "size": -1}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/efast/v1/entry-item":
		if !f.requireShareBearer(w, r) {
			return
		}
		writeJSON(w, []map[string]any{{
			"id": fixturePublicRootID, "docid": fixturePublicRootID, "name": "公开分享",
			"path": "/fixture/share/public", "type": "folder", "rev": "share-public-root-r1", "size": -1,
		}})
	case strings.HasPrefix(r.URL.Path, "/api/efast/"):
		if !f.requireDocumentBearer(w, r) {
			return
		}
		f.handleDocuments(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fixture) handleDocuments(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/efast/v1/folders/") && strings.HasSuffix(r.URL.Path, "/sub_objects"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/efast/v1/folders/"), "/sub_objects")
		if r.URL.Query().Get("limit") != "1000" {
			f.t.Errorf("folder query=%q", r.URL.RawQuery)
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		dirs, files := make([]map[string]any, 0), make([]map[string]any, 0)
		for _, node := range f.nodes {
			if node.ParentID != id {
				continue
			}
			if node.Type == "directory" {
				dirs = append(dirs, nodeWire(node))
			} else {
				files = append(files, nodeWire(node))
			}
		}
		f.mu.Unlock()
		sort.Slice(dirs, func(i, j int) bool { return dirs[i]["name"].(string) < dirs[j]["name"].(string) })
		sort.Slice(files, func(i, j int) bool { return files[i]["name"].(string) < files[j]["name"].(string) })
		writeJSON(w, map[string]any{"dirs": dirs, "files": files, "next_marker": ""})
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/dir/create":
		f.createDir(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/osoption":
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 {
			f.t.Errorf("osoption body=%q err=%v", body, err)
			http.Error(w, "unexpected body", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"partminsize": 204800, "partmaxsize": int64(5368709120), "partmaxnum": 10000})
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/predupload":
		var request struct {
			Length   int64  `json:"length"`
			SliceMD5 string `json:"slice_md5"`
		}
		if !decodeFixtureJSON(f, w, r, &request) {
			return
		}
		if request.Length < 0 || request.SliceMD5 == "" {
			f.t.Errorf("preupload request=%+v", request)
			http.Error(w, "bad preupload", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		expected, exists := f.expectedBySlice[request.SliceMD5]
		f.mu.Unlock()
		if !exists || expected.Size != request.Length {
			http.Error(w, "content evidence mismatch", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"match": expected.Instant})
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/dupload":
		f.directUpload(w, r)
	case r.Method == http.MethodPost && (r.URL.Path == "/api/efast/v1/file/osbeginupload" || r.URL.Path == "/api/efast/v1/file/osinitmultiupload"):
		f.beginUpload(w, r, strings.HasSuffix(r.URL.Path, "osinitmultiupload"))
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/osuploadpart":
		f.authorizeParts(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/oscompleteupload":
		f.completeMultipart(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/osuploadrefresh":
		f.refreshUpload(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/osendupload":
		f.finishUpload(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/metadata":
		f.fileMetadata(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/osdownload":
		f.authorizeDownload(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/file/delete":
		f.deleteFile(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/efast/v1/dir/delete":
		f.deleteDir(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fixture) createDir(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID string `json:"docid"`
		Name  string `json:"name"`
		OnDup int    `json:"ondup"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	if request.DocID == "" || request.Name == "" || request.OnDup != 1 {
		f.t.Errorf("mkdir request=%+v", request)
		http.Error(w, "bad mkdir", http.StatusBadRequest)
		return
	}
	drop := f.consumeFault("drop-create")
	f.mu.Lock()
	for _, node := range f.nodes {
		if node.ParentID == request.DocID && node.Name == request.Name {
			f.mu.Unlock()
			http.Error(w, "duplicate", http.StatusConflict)
			return
		}
	}
	parent := f.nodes[request.DocID]
	if parent == nil || parent.Type != "directory" {
		f.mu.Unlock()
		http.Error(w, "parent not found", http.StatusNotFound)
		return
	}
	f.seq++
	id := fmt.Sprintf("gns://dir-%d", f.seq)
	node := &fixtureNode{ID: id, ParentID: request.DocID, Name: request.Name, Type: "directory", Rev: "dir-r1", Path: strings.TrimRight(parent.Path, "/") + "/" + request.Name, Size: -1}
	f.nodes[id] = node
	f.mu.Unlock()
	if drop {
		dropHTTPConnection(f, w)
		return
	}
	writeJSON(w, nodeWire(node))
}

func (f *fixture) directUpload(w http.ResponseWriter, r *http.Request) {
	var request struct {
		CRC32       string `json:"crc32"`
		DocID       string `json:"docid"`
		Length      int64  `json:"length"`
		MD5         string `json:"md5"`
		ClientMtime int64  `json:"client_mtime"`
		Name        string `json:"name"`
		OnDup       int    `json:"ondup"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	if request.DocID == "" || request.Name == "" || request.OnDup != 1 || request.MD5 == "" || request.CRC32 == "" {
		f.t.Errorf("direct upload request=%+v", request)
		http.Error(w, "bad direct upload", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	expected, expectedOK := f.expectedByName[request.Name]
	data, ok := f.instant[request.Length]
	parent := f.nodes[request.DocID]
	if !ok || !expectedOK || !expected.Instant || parent == nil || request.Length != expected.Size || request.ClientMtime != expected.ClientMtimeUS || request.MD5 != expected.MD5 || request.CRC32 != expected.CRC32 || !bytes.Equal(data, expected.Data) {
		f.mu.Unlock()
		http.Error(w, "instant evidence mismatch", http.StatusBadRequest)
		return
	}
	f.seq++
	id := fmt.Sprintf("gns://file-%d", f.seq)
	node := &fixtureNode{ID: id, ParentID: request.DocID, Name: request.Name, Type: "file", Rev: "upload-r1", Path: strings.TrimRight(parent.Path, "/") + "/" + request.Name, Size: expected.Size, ClientMtimeUS: expected.ClientMtimeUS, MD5: expected.MD5, SliceMD5: expected.SliceMD5, CRC32: expected.CRC32}
	f.nodes[id] = node
	f.files[node.Path] = append([]byte(nil), data...)
	f.mu.Unlock()
	writeJSON(w, map[string]any{"docid": id, "rev": node.Rev, "name": node.Name})
}

func (f *fixture) beginUpload(w http.ResponseWriter, r *http.Request, multipartMode bool) {
	var request struct {
		DocID       string `json:"docid"`
		Length      int64  `json:"length"`
		Name        string `json:"name"`
		ClientMtime int64  `json:"client_mtime"`
		OnDup       int    `json:"ondup"`
		ReqMethod   string `json:"reqmethod"`
		EditedRev   string `json:"editedrev"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	if request.DocID == "" || request.Name == "" || request.Length < 0 || request.OnDup != 1 || request.EditedRev != "" || (!multipartMode && request.ReqMethod != http.MethodPut) || (multipartMode && request.ReqMethod != "") {
		f.t.Errorf("begin multipart=%v request=%+v", multipartMode, request)
		http.Error(w, "bad begin", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	expected, exists := f.expectedByName[request.Name]
	if !exists || expected.Size != request.Length || expected.ClientMtimeUS != request.ClientMtime {
		f.mu.Unlock()
		http.Error(w, "upload evidence mismatch", http.StatusBadRequest)
		return
	}
	f.seq++
	id := fmt.Sprintf("gns://file-%d", f.seq)
	rev := "upload-r1"
	uploadID := ""
	if multipartMode {
		uploadID = fmt.Sprintf("multipart-%d", f.seq)
	}
	f.pending[id] = &pendingUpload{DocID: id, ParentID: request.DocID, Name: request.Name, Rev: rev, UploadID: uploadID, Size: request.Length, ClientMtimeUS: request.ClientMtime, Parts: map[int][]byte{}, ETags: map[int]string{}, SingleGeneration: 1, MD5: expected.MD5, SliceMD5: expected.SliceMD5, CRC32: expected.CRC32}
	f.mu.Unlock()
	response := map[string]any{"docid": id, "rev": rev, "name": request.Name}
	if multipartMode {
		response["uploadid"] = uploadID
	} else {
		response["authrequest"] = []string{http.MethodPut, f.objects.URL + "/single?doc=" + url.QueryEscape(id) + "&signature=single-generation-1", "Authorization: AWS fixture-single-generation-1"}
	}
	writeJSON(w, response)
}

func (f *fixture) authorizeParts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID    string `json:"docid"`
		Rev      string `json:"rev"`
		UploadID string `json:"uploadid"`
		Parts    string `json:"parts"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	rangeParts := strings.Split(request.Parts, "-")
	if len(rangeParts) != 2 || rangeParts[0] != rangeParts[1] {
		f.t.Errorf("part authorization request=%+v", request)
		http.Error(w, "bad part range", http.StatusBadRequest)
		return
	}
	part, err := strconv.Atoi(rangeParts[0])
	if err != nil || strconv.Itoa(part) != rangeParts[0] || part < 1 {
		http.Error(w, "bad part", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	pending := f.pending[request.DocID]
	f.mu.Unlock()
	if pending == nil || pending.Rev != request.Rev || pending.UploadID != request.UploadID {
		http.Error(w, "bad upload identity", http.StatusNotFound)
		return
	}
	signedURL := fmt.Sprintf("%s/part?doc=%s&part=%d&signature=part-%s-%d", f.objects.URL, url.QueryEscape(request.DocID), part, url.QueryEscape(request.UploadID), part)
	signedHeader := fmt.Sprintf("Authorization: AWS fixture-part-%s-%d", request.UploadID, part)
	writeJSON(w, map[string]any{strconv.Itoa(part): []string{http.MethodPut, signedURL, signedHeader}})
}

func (f *fixture) completeMultipart(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID    string         `json:"docid"`
		Rev      string         `json:"rev"`
		UploadID string         `json:"uploadid"`
		PartInfo map[string]any `json:"partinfo"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	f.mu.Lock()
	pending := f.pending[request.DocID]
	valid := pending != nil && pending.Rev == request.Rev && pending.UploadID == request.UploadID && len(request.PartInfo) == len(pending.Parts)
	completionParts := make([]completionPart, 0, len(request.PartInfo))
	if valid {
		for rawPart, rawInfo := range request.PartInfo {
			part, err := strconv.Atoi(rawPart)
			info, ok := rawInfo.([]any)
			if err != nil || !ok || len(info) != 2 {
				valid = false
				break
			}
			etag, etagOK := info[0].(string)
			size, sizeOK := info[1].(float64)
			if !etagOK || !sizeOK || etag != pending.ETags[part] || int64(size) != int64(len(pending.Parts[part])) {
				valid = false
				break
			}
			completionParts = append(completionParts, completionPart{Number: part, ETag: etag, Size: int64(size)})
		}
	}
	sort.Slice(completionParts, func(i, j int) bool { return completionParts[i].Number < completionParts[j].Number })
	if valid {
		var reconstructed []byte
		for _, part := range completionParts {
			reconstructed = append(reconstructed, pending.Parts[part.Number]...)
		}
		expected, exists := f.expectedByName[pending.Name]
		valid = exists && bytes.Equal(reconstructed, expected.Data)
	}
	if !valid {
		f.mu.Unlock()
		f.t.Errorf("multipart completion request=%+v", request)
		http.Error(w, "bad completion", http.StatusBadRequest)
		return
	}
	completionXML, err := xml.Marshal(completionDocument{UploadID: request.UploadID, Parts: completionParts})
	if err != nil {
		f.mu.Unlock()
		f.t.Errorf("marshal completion XML: %v", err)
		http.Error(w, "completion XML", http.StatusInternalServerError)
		return
	}
	pending.CompletionXML = append([]byte(nil), completionXML...)
	f.mu.Unlock()
	var response bytes.Buffer
	writer := multipart.NewWriter(&response)
	_ = writer.SetBoundary("fixture-boundary")
	xmlHeader := make(textproto.MIMEHeader)
	xmlHeader.Set("Content-Type", "application/xml")
	xmlPart, _ := writer.CreatePart(xmlHeader)
	_, _ = xmlPart.Write(completionXML)
	jsonHeader := make(textproto.MIMEHeader)
	jsonHeader.Set("Content-Type", "application/json")
	jsonPart, _ := writer.CreatePart(jsonHeader)
	_ = json.NewEncoder(jsonPart).Encode(map[string]any{"authrequest": []string{http.MethodPut, f.objects.URL + "/complete?doc=" + url.QueryEscape(request.DocID) + "&signature=complete-" + url.QueryEscape(request.UploadID), "Authorization: AWS fixture-complete-" + request.UploadID}})
	_ = writer.Close()
	w.Header().Set("Content-Type", writer.FormDataContentType())
	_, _ = w.Write(response.Bytes())
}

func (f *fixture) refreshUpload(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID     string `json:"docid"`
		Rev       string `json:"rev"`
		Length    int64  `json:"length"`
		Multi     bool   `json:"multiupload"`
		ReqMethod string `json:"reqmethod"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	f.mu.Lock()
	pending := f.pending[request.DocID]
	f.mu.Unlock()
	if pending == nil || pending.Rev != request.Rev || pending.Size != request.Length {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}
	if request.Multi {
		if request.ReqMethod != "" || pending.UploadID == "" {
			http.Error(w, "bad multipart refresh", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"uploadid": pending.UploadID})
		return
	}
	if request.ReqMethod != http.MethodPut || pending.UploadID != "" {
		http.Error(w, "bad single refresh", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	pending.SingleGeneration = 2
	f.mu.Unlock()
	writeJSON(w, map[string]any{"authrequest": []string{http.MethodPut, f.objects.URL + "/single?doc=" + url.QueryEscape(request.DocID) + "&signature=single-generation-2", "Authorization: AWS fixture-single-generation-2"}})
}

func (f *fixture) finishUpload(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID     string `json:"docid"`
		Rev       string `json:"rev"`
		CRC32     string `json:"crc32"`
		MD5       string `json:"md5"`
		SliceMD5  string `json:"slice_md5"`
		EditedRev string `json:"editedrev"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	if request.CRC32 != strings.ToUpper(request.CRC32) || request.MD5 != strings.ToUpper(request.MD5) || request.SliceMD5 != strings.ToUpper(request.SliceMD5) {
		f.t.Errorf("finish hashes not uppercase: %+v", request)
		http.Error(w, "bad checksums", http.StatusBadRequest)
		return
	}
	drop := f.consumeFault("drop-finish")
	f.mu.Lock()
	pending := f.pending[request.DocID]
	if pending == nil || pending.Rev != request.Rev || int64(len(pending.Data)) != pending.Size {
		f.mu.Unlock()
		http.Error(w, "upload data incomplete", http.StatusPreconditionFailed)
		return
	}
	computed := contentEvidence(pending.Name, pending.Data, pending.ClientMtimeUS, false)
	expected, exists := f.expectedByName[pending.Name]
	if !exists || !bytes.Equal(pending.Data, expected.Data) || pending.ClientMtimeUS != expected.ClientMtimeUS || request.MD5 != computed.MD5 || request.SliceMD5 != computed.SliceMD5 || request.CRC32 != computed.CRC32 {
		f.mu.Unlock()
		http.Error(w, "content evidence mismatch", http.StatusBadRequest)
		return
	}
	parent := f.nodes[pending.ParentID]
	path := strings.TrimRight(parent.Path, "/") + "/" + pending.Name
	node := &fixtureNode{ID: pending.DocID, ParentID: pending.ParentID, Name: pending.Name, Type: "file", Rev: pending.Rev, Path: path, Size: pending.Size, ClientMtimeUS: expected.ClientMtimeUS, MD5: computed.MD5, SliceMD5: computed.SliceMD5, CRC32: computed.CRC32}
	f.nodes[node.ID] = node
	f.files[path] = append([]byte(nil), pending.Data...)
	delete(f.pending, request.DocID)
	f.mu.Unlock()
	if drop {
		dropHTTPConnection(f, w)
		return
	}
	writeJSON(w, map[string]any{"docid": node.ID, "rev": node.Rev, "name": node.Name})
}

func (f *fixture) fileMetadata(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID string `json:"docid"`
		Rev   string `json:"rev"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	mismatch := f.consumeFault("metadata-mismatch")
	f.mu.Lock()
	stored := f.nodes[request.DocID]
	var node *fixtureNode
	if stored != nil {
		copyNode := *stored
		node = &copyNode
	}
	f.mu.Unlock()
	if node == nil || node.Rev != request.Rev {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if mismatch {
		node.Size++
	}
	writeJSON(w, nodeWire(node))
}

func (f *fixture) authorizeDownload(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID    string `json:"docid"`
		AuthType string `json:"authtype"`
		SaveName string `json:"savename"`
		UseHTTPS bool   `json:"usehttps"`
		Rev      string `json:"rev"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	f.mu.Lock()
	node := f.nodes[request.DocID]
	_, dataExists := f.files[nodePath(node)]
	f.mu.Unlock()
	if node == nil || node.Type != "file" || !dataExists || request.AuthType != "QUERY_STRING" || request.SaveName != node.Name || !request.UseHTTPS || request.Rev != node.Rev {
		f.t.Errorf("download authorization request=%+v node=%+v", request, node)
		http.Error(w, "bad download authorization", http.StatusBadRequest)
		return
	}
	signedURL := f.objects.URL + "/download?doc=" + url.QueryEscape(node.ID) + "&rev=" + url.QueryEscape(node.Rev) + "&signature=fixture-download"
	writeJSON(w, map[string]any{"authrequest": []string{
		http.MethodGet,
		signedURL,
		"Authorization: AWS must-not-leave-control-plane",
		"x-as-userid: fixture-user",
	}})
}

func (f *fixture) deleteFile(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID string `json:"docid"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	if f.consumeFault("delete-pending") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	f.mu.Lock()
	node := f.nodes[request.DocID]
	if node == nil || node.Type != "file" {
		f.mu.Unlock()
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	delete(f.files, node.Path)
	delete(f.nodes, request.DocID)
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fixture) deleteDir(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DocID              string `json:"docid"`
		CheckUploadProcess bool   `json:"check_upload_process"`
	}
	if !decodeFixtureJSON(f, w, r, &request) {
		return
	}
	if !request.CheckUploadProcess {
		f.t.Errorf("directory delete omitted upload-process guard: %+v", request)
		http.Error(w, "missing upload guard", http.StatusBadRequest)
		return
	}
	if f.consumeFault("delete-pending") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	f.mu.Lock()
	node := f.nodes[request.DocID]
	if node == nil || node.Type != "directory" || request.DocID == "gns://library" {
		f.mu.Unlock()
		http.Error(w, "directory not found", http.StatusNotFound)
		return
	}
	remove := map[string]bool{request.DocID: true}
	for changed := true; changed; {
		changed = false
		for id, candidate := range f.nodes {
			if !remove[id] && remove[candidate.ParentID] {
				remove[id] = true
				changed = true
			}
		}
	}
	for id := range remove {
		if candidate := f.nodes[id]; candidate != nil {
			delete(f.files, candidate.Path)
		}
		delete(f.nodes, id)
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func nodePath(node *fixtureNode) string {
	if node == nil {
		return ""
	}
	return node.Path
}

func (f *fixture) handleObject(w http.ResponseWriter, r *http.Request) {
	f.recordCall("object:" + r.URL.Path)
	authorization := r.Header.Get("Authorization")
	if r.Method == http.MethodGet && r.URL.Path == "/download" {
		f.downloadObject(w, r, authorization)
		return
	}
	if r.Method != http.MethodPut || strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		http.Error(w, "bad signed request", http.StatusUnauthorized)
		return
	}
	docID := r.URL.Query().Get("doc")
	f.mu.Lock()
	pending := f.pending[docID]
	f.mu.Unlock()
	if pending == nil {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}
	validAuthorization := false
	switch r.URL.Path {
	case "/single":
		validAuthorization = pending.SingleGeneration > 0 && authorization == fmt.Sprintf("AWS fixture-single-generation-%d", pending.SingleGeneration) && r.URL.Query().Get("signature") == fmt.Sprintf("single-generation-%d", pending.SingleGeneration)
	case "/part":
		part := r.URL.Query().Get("part")
		validAuthorization = authorization == "AWS fixture-part-"+pending.UploadID+"-"+part && r.URL.Query().Get("signature") == "part-"+pending.UploadID+"-"+part
	case "/complete":
		validAuthorization = authorization == "AWS fixture-complete-"+pending.UploadID && r.URL.Query().Get("signature") == "complete-"+pending.UploadID
	}
	if !validAuthorization {
		http.Error(w, "bad signed request", http.StatusUnauthorized)
		return
	}
	if f.consumeFault("signed-expired") {
		if r.URL.Path == "/single" {
			f.mu.Lock()
			pending.SingleGeneration = -1
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>ExpiredToken</Code><Message>request has expired</Message></Error>")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if r.ContentLength != int64(len(body)) {
		f.t.Errorf("object %s content-length=%d body=%d", r.URL.Path, r.ContentLength, len(body))
		http.Error(w, "bad content length", http.StatusBadRequest)
		return
	}
	switch r.URL.Path {
	case "/single":
		f.mu.Lock()
		pending.Data = append([]byte(nil), body...)
		f.puts[pending.Name]++
		f.mu.Unlock()
		w.Header().Set("ETag", `"single-etag"`)
	case "/part":
		part, err := strconv.Atoi(r.URL.Query().Get("part"))
		if err != nil || part < 1 {
			http.Error(w, "bad part", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.partTry[part]++
		if f.gateTwo && len(f.partOK) >= 2 {
			f.twoOnce.Do(func() { close(f.twoDone) })
			f.mu.Unlock()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Second):
				http.Error(w, "gate timeout", http.StatusGatewayTimeout)
				return
			}
		}
		etag := fmt.Sprintf(`"part-%d-etag"`, part)
		pending.Parts[part] = append([]byte(nil), body...)
		pending.ETags[part] = etag
		f.parts[part] = append([]byte(nil), body...)
		f.puts[pending.Name]++
		f.partOK[part] = true
		f.mu.Unlock()
		w.Header().Set("ETag", etag)
	case "/complete":
		f.mu.Lock()
		if !bytes.Equal(body, pending.CompletionXML) {
			f.mu.Unlock()
			http.Error(w, "completion XML mismatch", http.StatusBadRequest)
			return
		}
		var completion completionDocument
		if err := xml.Unmarshal(body, &completion); err != nil || completion.UploadID != pending.UploadID || len(completion.Parts) != len(pending.Parts) {
			f.mu.Unlock()
			http.Error(w, "invalid completion XML", http.StatusBadRequest)
			return
		}
		for _, part := range completion.Parts {
			if part.Number < 1 || part.ETag != pending.ETags[part.Number] || part.Size != int64(len(pending.Parts[part.Number])) {
				f.mu.Unlock()
				http.Error(w, "completion part mismatch", http.StatusBadRequest)
				return
			}
		}
		numbers := make([]int, 0, len(pending.Parts))
		for part := range pending.Parts {
			numbers = append(numbers, part)
		}
		sort.Ints(numbers)
		pending.Data = pending.Data[:0]
		for _, part := range numbers {
			pending.Data = append(pending.Data, pending.Parts[part]...)
		}
		f.mu.Unlock()
		w.Header().Set("ETag", `"complete-etag"`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fixture) downloadObject(w http.ResponseWriter, r *http.Request, authorization string) {
	if authorization != "" || r.Header.Get("Cookie") != "" || r.Header.Get("x-as-userid") != "" || r.URL.Query().Get("signature") != "fixture-download" {
		http.Error(w, "bad signed download", http.StatusUnauthorized)
		return
	}
	docID := r.URL.Query().Get("doc")
	rev := r.URL.Query().Get("rev")
	f.mu.Lock()
	node := f.nodes[docID]
	var data []byte
	if node != nil {
		data = append([]byte(nil), f.files[node.Path]...)
	}
	f.mu.Unlock()
	if node == nil || node.Type != "file" || node.Rev != rev {
		http.Error(w, "download not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func decodeFixtureJSON(f *fixture, w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		f.t.Errorf("decode %s: %v", r.URL.Path, err)
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func (f *fixture) consumeFault(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.faults[name] <= 0 {
		return false
	}
	f.faults[name]--
	return true
}

func dropHTTPConnection(f *fixture, w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		f.t.Errorf("response writer does not support HTTP/1.1 hijacking")
		return
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		f.t.Errorf("hijack committed response: %v", err)
		return
	}
	_ = connection.Close()
}

func nodeWire(node *fixtureNode) map[string]any {
	return map[string]any{
		"id": node.ID, "docid": node.ID, "name": node.Name, "path": node.Path, "type": node.Type,
		"rev": node.Rev, "size": node.Size, "client_mtime": node.ClientMtimeUS, "md5": node.MD5,
		"slice_md5": node.SliceMD5, "crc32": node.CRC32,
	}
}

func (f *fixture) requireBearer(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	access := f.activeAccess
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+access {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fixture) requireShareBearer(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+fixtureShareToken || r.Header.Get("Cookie") != "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fixture) requireDocumentBearer(w http.ResponseWriter, r *http.Request) bool {
	docID := fixtureDocumentID(r)
	if isFixturePublicDocument(docID) {
		return f.requireShareBearer(w, r)
	}
	return f.requireBearer(w, r)
}

func fixtureDocumentID(r *http.Request) string {
	const folderPrefix = "/api/efast/v1/folders/"
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, folderPrefix) && strings.HasSuffix(r.URL.Path, "/sub_objects") {
		raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, folderPrefix), "/sub_objects")
		if decoded, err := url.PathUnescape(raw); err == nil {
			return decoded
		}
		return raw
	}
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var request struct {
		DocID string `json:"docid"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	return request.DocID
}

func isFixturePublicDocument(docID string) bool {
	switch docID {
	case fixturePublicRootID, fixturePublicDirID, fixturePublicFileID, fixturePublicTopID:
		return true
	default:
		return false
	}
}

func (f *fixture) recordCall(route string) {
	f.mu.Lock()
	f.calls[route]++
	f.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestFixtureStartsSeparateTLSPlanes(t *testing.T) {
	f := newFixture(t)
	if !strings.HasPrefix(f.control.URL, "https://") || !strings.HasPrefix(f.objects.URL, "https://") {
		t.Fatalf("control=%q objects=%q", f.control.URL, f.objects.URL)
	}
	if f.control.URL == f.objects.URL {
		t.Fatal("control and object planes must differ")
	}
}

func TestFixtureRotatesBearerAndSignedAuthorization(t *testing.T) {
	t.Run("bearer token", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		loginCLI(t, r)
		f.setFault("entry-401", 1)
		runJSON(t, r, []string{"ls", "/", "--json"}, 0)

		endpoint := f.control.URL + "/api/efast/v1/entry-doc-lib?direction=asc&sort=doc_lib_name"
		if status := authorizedStatus(t, f.control.Client(), endpoint, fixtureAccess); status != http.StatusUnauthorized {
			t.Fatalf("stale bearer status=%d want=%d", status, http.StatusUnauthorized)
		}
		if status := authorizedStatus(t, f.control.Client(), endpoint, fixtureRefreshedAccess); status != http.StatusOK {
			t.Fatalf("refreshed bearer status=%d want=%d", status, http.StatusOK)
		}
	})

	t.Run("signed request", func(t *testing.T) {
		f := newFixture(t)
		const docID = "gns://signed-rotation"
		f.mu.Lock()
		f.pending[docID] = &pendingUpload{DocID: docID, ParentID: "gns://library", Name: "signed.bin", Rev: "upload-r1", Size: 7, Parts: map[int][]byte{}, ETags: map[int]string{}, SingleGeneration: 1}
		f.mu.Unlock()
		f.setFault("signed-expired", 1)

		initialURL := f.objects.URL + "/single?doc=" + url.QueryEscape(docID) + "&signature=single-generation-1"
		if status := signedPutStatus(t, f.objects.Client(), initialURL, "AWS fixture-single-generation-1", []byte("payload")); status != http.StatusForbidden {
			t.Fatalf("expired signed status=%d want=%d", status, http.StatusForbidden)
		}
		if status := signedPutStatus(t, f.objects.Client(), initialURL, "AWS fixture-single-generation-1", []byte("payload")); status != http.StatusUnauthorized {
			t.Fatalf("stale signed status=%d want=%d", status, http.StatusUnauthorized)
		}
		refreshBody := []byte(`{"docid":"gns://signed-rotation","rev":"upload-r1","length":7,"multiupload":false,"reqmethod":"PUT"}`)
		if status := controlJSONStatus(t, f.control.Client(), f.control.URL+"/api/efast/v1/file/osuploadrefresh", fixtureAccess, refreshBody); status != http.StatusOK {
			t.Fatalf("signed refresh status=%d want=%d", status, http.StatusOK)
		}
		refreshedURL := f.objects.URL + "/single?doc=" + url.QueryEscape(docID) + "&signature=single-generation-2"
		if status := signedPutStatus(t, f.objects.Client(), refreshedURL, "AWS fixture-single-generation-2", []byte("payload")); status != http.StatusOK {
			t.Fatalf("refreshed signed status=%d want=%d", status, http.StatusOK)
		}
	})
}

func TestFixtureRejectsIncorrectUploadEvidence(t *testing.T) {
	f := newFixture(t)
	data := []byte("independent fixture payload")
	f.registerExpected(contentEvidence("wrong.bin", data, 1, true))
	f.registerExpected(contentEvidence("oracle.bin", data, 1, false))

	preupload := fmt.Sprintf(`{"length":%d,"slice_md5":"WRONG-SLICE"}`, len(data))
	if status := controlJSONStatus(t, f.control.Client(), f.control.URL+"/api/efast/v1/file/predupload", fixtureAccess, []byte(preupload)); status != http.StatusBadRequest {
		t.Fatalf("wrong preupload evidence status=%d want=%d", status, http.StatusBadRequest)
	}
	direct := fmt.Sprintf(`{"crc32":"WRONG","docid":"gns://library","length":%d,"md5":"WRONG","client_mtime":1,"name":"wrong.bin","ondup":1}`, len(data))
	if status := controlJSONStatus(t, f.control.Client(), f.control.URL+"/api/efast/v1/file/dupload", fixtureAccess, []byte(direct)); status != http.StatusBadRequest {
		t.Fatalf("wrong direct evidence status=%d want=%d", status, http.StatusBadRequest)
	}

	const docID = "gns://oracle-multipart"
	f.mu.Lock()
	f.pending[docID] = &pendingUpload{DocID: docID, ParentID: "gns://library", Name: "oracle.bin", Rev: "upload-r1", UploadID: "multipart-oracle", Size: int64(len(data)), ClientMtimeUS: 1, Data: append([]byte(nil), data...), Parts: map[int][]byte{1: append([]byte(nil), data...)}, ETags: map[int]string{1: `"part-1-etag"`}}
	f.mu.Unlock()
	completion := []byte(`{"docid":"gns://oracle-multipart","rev":"upload-r1","uploadid":"multipart-oracle","partinfo":{"1":["\"part-1-etag\"",27]}}`)
	if status := controlJSONStatus(t, f.control.Client(), f.control.URL+"/api/efast/v1/file/oscompleteupload", fixtureAccess, completion); status != http.StatusOK {
		t.Fatalf("completion authorization status=%d want=%d", status, http.StatusOK)
	}
	completeURL := f.objects.URL + "/complete?doc=" + url.QueryEscape(docID) + "&signature=complete-multipart-oracle"
	if status := signedPutStatus(t, f.objects.Client(), completeURL, "AWS fixture-complete-multipart-oracle", []byte("<wrong/>")); status != http.StatusBadRequest {
		t.Fatalf("wrong completion XML status=%d want=%d", status, http.StatusBadRequest)
	}
	finish := []byte(`{"docid":"gns://oracle-multipart","rev":"upload-r1","crc32":"WRONG","md5":"WRONG","slice_md5":"WRONG"}`)
	if status := controlJSONStatus(t, f.control.Client(), f.control.URL+"/api/efast/v1/file/osendupload", fixtureAccess, finish); status != http.StatusBadRequest {
		t.Fatalf("wrong finish evidence status=%d want=%d", status, http.StatusBadRequest)
	}
}

func TestSecretMaterialScanIsCaseInsensitive(t *testing.T) {
	for _, material := range []string{
		"authorization: aws fixture-part-upload-1-2",
		"COOKIE: session=synthetic",
		"Fixture-Access-Refreshed",
		"HTTPS://OBJECTS.INVALID/item?SIGNATURE=synthetic",
	} {
		if !containsSecretMaterial([]byte(material), "https://objects.invalid") {
			t.Fatalf("secret scan accepted %q", material)
		}
	}
	if containsSecretMaterial([]byte(`{"phase":"uploading","remote_path":"/个人文档/file.bin"}`), "https://objects.invalid") {
		t.Fatal("secret scan rejected ordinary state")
	}
}

func TestAwaitCLIOutcomeTimesOut(t *testing.T) {
	if _, ok := awaitCLIOutcome(make(chan cliOutcome), time.Millisecond); ok {
		t.Fatal("empty outcome channel unexpectedly completed")
	}
}

func authorizedStatus(t *testing.T, client *http.Client, endpoint, token string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return requestStatus(t, client, request)
}

func controlJSONStatus(t *testing.T, client *http.Client, endpoint, token string, body []byte) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	return requestStatus(t, client, request)
}

func signedPutStatus(t *testing.T, client *http.Client, endpoint, authorization string, body []byte) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	return requestStatus(t, client, request)
}

func requestStatus(t *testing.T, client *http.Client, request *http.Request) int {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode
}

func TestCLILoginAndStatus(t *testing.T) {
	f := newFixture(t)
	r := newRuntime(t, f)

	var loginOut, loginErr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"login"}, strings.NewReader(""), &loginOut, &loginErr, r.deps, version.Info{}); code != 0 {
		t.Fatalf("login exit=%d stdout=%q stderr=%q", code, loginOut.String(), loginErr.String())
	}
	var statusOut, statusErr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"status", "--json"}, strings.NewReader(""), &statusOut, &statusErr, r.deps, version.Info{}); code != 0 {
		t.Fatalf("status exit=%d stdout=%q stderr=%q", code, statusOut.String(), statusErr.String())
	}
	var got struct {
		OK      bool   `json:"ok"`
		Account string `json:"account"`
	}
	if err := json.Unmarshal(statusOut.Bytes(), &got); err != nil {
		t.Fatalf("decode status JSON: %v; stdout=%q", err, statusOut.String())
	}
	if !got.OK || got.Account != "fixture-user" {
		t.Fatalf("status=%+v", got)
	}
	info, err := os.Stat(r.paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode=%#o", info.Mode().Perm())
	}
	credentialBytes, err := os.ReadFile(r.paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	combined := loginOut.String() + loginErr.String() + statusOut.String() + statusErr.String()
	for _, secret := range []string{fixtureAccess, fixtureRefresh, fixtureSecret, f.objects.URL} {
		if strings.Contains(combined, secret) {
			t.Fatalf("command output leaked %q: %q", secret, combined)
		}
	}
	if strings.Contains(string(credentialBytes), f.objects.URL) {
		t.Fatalf("credentials contain object-plane URL: %s", credentialBytes)
	}
}

func TestCLIUploadModes(t *testing.T) {
	f := newFixture(t)
	r := newRuntime(t, f)
	loginCLI(t, r)

	instant := bytes.Repeat([]byte("instant-content-"), 64)
	single := bytes.Repeat([]byte("single-content-"), 64<<10)
	multipart := make([]byte, 20<<20)
	for i := range multipart {
		multipart[i] = byte((i*31 + 7) % 251)
	}
	runJSON(t, r, []string{"mkdir", "/个人文档/CLI测试/uploads", "--parents", "--json"}, 0)
	cases := []struct {
		name      string
		payload   []byte
		instant   bool
		multipart bool
		puts      int
	}{
		{name: "instant.bin", payload: instant, instant: true, puts: 0},
		{name: "single.bin", payload: single, puts: 1},
		{name: "multipart.bin", payload: multipart, multipart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(local, tc.payload, 0o600); err != nil {
				t.Fatal(err)
			}
			f.expectFile(t, local, tc.name, tc.payload, tc.instant)
			remotePath := "/个人文档/CLI测试/uploads/" + tc.name
			result := runJSON(t, r, []string{"put", local, remotePath, "--json"}, 0)
			if result["remote_path"] != remotePath || result["remote_id"] == "" || int64(result["size"].(float64)) != int64(len(tc.payload)) || result["instant"] != tc.instant || result["multipart"] != tc.multipart {
				t.Fatalf("put result=%v", result)
			}
			if tc.puts >= 0 && !tc.multipart && f.objectPutCount(tc.name) != tc.puts {
				t.Fatalf("object PUTs for %s=%d want=%d", tc.name, f.objectPutCount(tc.name), tc.puts)
			}
			if tc.multipart {
				if f.objectPutCount(tc.name) != 5 || int(result["parts_total"].(float64)) != 5 || int(result["parts_uploaded"].(float64)) != 5 {
					t.Fatalf("multipart PUTs=%d result=%v", f.objectPutCount(tc.name), result)
				}
			}
			if got := f.fileBytes(remotePath); !bytes.Equal(got, tc.payload) {
				t.Fatalf("published bytes for %s: got=%d want=%d", remotePath, len(got), len(tc.payload))
			}
		})
	}
	listing := runJSON(t, r, []string{"ls", "/个人文档/CLI测试/uploads", "--json"}, 0)
	entries, ok := listing["entries"].([]any)
	if !ok || len(entries) != len(cases) {
		t.Fatalf("ls entries=%T %v", listing["entries"], listing["entries"])
	}
	wantSizes := map[string]int64{}
	for _, tc := range cases {
		wantSizes["/个人文档/CLI测试/uploads/"+tc.name] = int64(len(tc.payload))
	}
	for _, raw := range entries {
		entry := raw.(map[string]any)
		path, _ := entry["remote_path"].(string)
		wantSize, exists := wantSizes[path]
		if !exists || entry["remote_id"] == "" || entry["type"] != "file" || int64(entry["size"].(float64)) != wantSize || entry["name"] != filepath.Base(path) {
			t.Fatalf("ls entry=%v", entry)
		}
	}
}

func TestCLIGetAndDelete(t *testing.T) {
	f := newFixture(t)
	r := newRuntime(t, f)
	loginCLI(t, r)

	const base = "/个人文档/CLI测试/transfers"
	runJSON(t, r, []string{"mkdir", base, "--parents", "--json"}, 0)
	local, payload := writePayload(t, f, "round-trip.tsv", 128<<10)
	remotePath := base + "/round-trip.tsv"
	runJSON(t, r, []string{"put", local, remotePath, "--json"}, 0)

	destination := filepath.Join(t.TempDir(), "downloaded.tsv")
	result := runJSON(t, r, []string{"get", remotePath, destination, "--json"}, 0)
	absDestination, err := filepath.Abs(destination)
	if err != nil {
		t.Fatal(err)
	}
	if result["remote_path"] != remotePath || result["remote_id"] == "" || result["revision"] == "" || result["local_path"] != absDestination || int64(result["size"].(float64)) != int64(len(payload)) {
		t.Fatalf("get result=%v", result)
	}
	if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("downloaded bytes=%d err=%v want=%d", len(got), err, len(payload))
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("download mode=%v err=%v want=0600", infoMode(info), err)
	}

	conflict := runJSON(t, r, []string{"get", remotePath, destination, "--json"}, 6)
	if conflict["ok"] != false || conflict["category"] != "local" {
		t.Fatalf("get conflict=%v", conflict)
	}
	if err := os.WriteFile(destination, []byte("stale local data"), 0o600); err != nil {
		t.Fatal(err)
	}
	overwritten := runJSON(t, r, []string{"get", remotePath, destination, "--overwrite", "--json"}, 0)
	if overwritten["local_path"] != absDestination {
		t.Fatalf("overwrite result=%v", overwritten)
	}
	if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("overwritten bytes=%d err=%v want=%d", len(got), err, len(payload))
	}

	deleted := runJSON(t, r, []string{"rm", remotePath, "--json"}, 0)
	if deleted["remote_path"] != remotePath || deleted["remote_id"] == "" || deleted["type"] != "file" || deleted["status"] != "deleted" || deleted["pending_review"] != false {
		t.Fatalf("file delete result=%v", deleted)
	}
	assertListingExcludes(t, runJSON(t, r, []string{"ls", base, "--json"}, 0), remotePath)

	const tree = base + "/tree"
	runJSON(t, r, []string{"mkdir", tree + "/child", "--parents", "--json"}, 0)
	withoutRecursive := runJSON(t, r, []string{"rm", tree, "--json"}, 4)
	if withoutRecursive["ok"] != false || withoutRecursive["category"] != "remote" {
		t.Fatalf("directory delete without --recursive=%v", withoutRecursive)
	}
	assertListingIncludes(t, runJSON(t, r, []string{"ls", base, "--json"}, 0), tree)
	deletedTree := runJSON(t, r, []string{"rm", tree, "--recursive", "--json"}, 0)
	if deletedTree["remote_path"] != tree || deletedTree["type"] != "directory" || deletedTree["status"] != "deleted" || deletedTree["pending_review"] != false {
		t.Fatalf("directory delete result=%v", deletedTree)
	}
	assertListingExcludes(t, runJSON(t, r, []string{"ls", base, "--json"}, 0), tree)

	pendingLocal, _ := writePayload(t, f, "pending-review.bin", 4096)
	pendingPath := base + "/pending-review.bin"
	runJSON(t, r, []string{"put", pendingLocal, pendingPath, "--json"}, 0)
	f.setFault("delete-pending", 1)
	pending := runJSON(t, r, []string{"rm", pendingPath, "--json"}, 0)
	if pending["remote_path"] != pendingPath || pending["type"] != "file" || pending["status"] != "pending_review" || pending["pending_review"] != true {
		t.Fatalf("pending-review delete result=%v", pending)
	}
	assertListingIncludes(t, runJSON(t, r, []string{"ls", base, "--json"}, 0), pendingPath)
}

func TestCLIShareLinkDownload(t *testing.T) {
	t.Run("anonymous folder works without saved credentials", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		link := f.control.URL + "/link/" + fixturePublicLinkID

		listing := runJSON(t, r, []string{"ls", "--share", link, "--json"}, 0)
		if listing["source"] != "share" {
			t.Fatalf("share listing=%v", listing)
		}
		entries, ok := listing["entries"].([]any)
		if !ok || len(entries) != 2 {
			t.Fatalf("share entries=%T %v", listing["entries"], listing["entries"])
		}
		wantFirstLevel := map[string]string{"sub": "directory", "top.txt": "file"}
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("share entry=%T %v", raw, raw)
			}
			path, _ := entry["share_path"].(string)
			if entry["type"] != wantFirstLevel[path] || entry["name"] != path || strings.Contains(path, "nested.txt") {
				t.Fatalf("first-level share entry=%v", entry)
			}
		}

		destination := t.TempDir()
		result := runJSON(t, r, []string{"get", "--share", link, destination, "sub/nested.txt", "--json"}, 0)
		if result["source"] != "share" {
			t.Fatalf("share download=%v", result)
		}
		downloads, ok := result["downloads"].([]any)
		if !ok || len(downloads) != 1 {
			t.Fatalf("share downloads=%T %v", result["downloads"], result["downloads"])
		}
		downloaded := downloads[0].(map[string]any)
		localPath := filepath.Join(destination, "sub", "nested.txt")
		if downloaded["share_path"] != "sub/nested.txt" || downloaded["local_path"] != localPath || downloaded["revision"] != "share-public-file-r1" {
			t.Fatalf("share download result=%v", downloaded)
		}
		want := f.fileBytes("/fixture/share/public/sub/nested.txt")
		if got, err := os.ReadFile(localPath); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("anonymous share bytes=%q err=%v want=%q", got, err, want)
		}
		if info, err := os.Stat(localPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("anonymous share mode=%v err=%v want=0600", infoMode(info), err)
		}
		if info, err := os.Stat(filepath.Dir(localPath)); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("anonymous share parent mode=%v err=%v want=0700", infoMode(info), err)
		}

		if got := f.callCount("/api/efast/v1/entry-item"); got != 2 {
			t.Fatalf("anonymous entry-item calls=%d want=2", got)
		}
		if got := f.callCount(fixtureFolderRoute(fixturePublicRootID)); got != 2 {
			t.Fatalf("anonymous root list calls=%d want=2", got)
		}
		if got := f.callCount(fixtureFolderRoute(fixturePublicDirID)); got != 1 {
			t.Fatalf("anonymous nested list calls=%d want=1", got)
		}
		if got := f.callCount("/api/efast/v1/file/metadata"); got != 1 {
			t.Fatalf("anonymous metadata calls=%d want=1", got)
		}
		if got := f.callCount("/api/efast/v1/file/osdownload"); got != 1 {
			t.Fatalf("anonymous download authorization calls=%d want=1", got)
		}
		if got := f.callCount("object:/download"); got != 1 {
			t.Fatalf("anonymous object calls=%d want=1", got)
		}
	})

	t.Run("realname folder requires and then uses saved login", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		link := f.control.URL + "/link/" + fixtureRealnameLinkID
		destination := t.TempDir()

		unauthorized := runJSON(t, r, []string{"get", "--share", link, destination, "member.txt", "--json"}, 3)
		if unauthorized["ok"] != false || unauthorized["category"] != "auth" {
			t.Fatalf("realname without login=%v", unauthorized)
		}
		if got := f.callCountPrefix("/api/efast/"); got != 0 {
			t.Fatalf("realname without login sent %d document requests", got)
		}
		if got := f.callCount("object:/download"); got != 0 {
			t.Fatalf("realname without login sent %d object requests", got)
		}

		loginCLI(t, r)
		result := runJSON(t, r, []string{"get", "--share", link, destination, "member.txt", "--json"}, 0)
		downloads, ok := result["downloads"].([]any)
		if result["source"] != "share" || !ok || len(downloads) != 1 {
			t.Fatalf("realname download=%v", result)
		}
		downloaded := downloads[0].(map[string]any)
		localPath := filepath.Join(destination, "member.txt")
		if downloaded["share_path"] != "member.txt" || downloaded["local_path"] != localPath || downloaded["revision"] != "share-real-file-r1" {
			t.Fatalf("realname download result=%v", downloaded)
		}
		want := f.fileBytes("/fixture/share/real/member.txt")
		if got, err := os.ReadFile(localPath); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("realname share bytes=%q err=%v want=%q", got, err, want)
		}
		if info, err := os.Stat(localPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("realname share mode=%v err=%v want=0600", infoMode(info), err)
		}
		if got := f.callCount("/api/efast/v1/entry-item"); got != 0 {
			t.Fatalf("realname used anonymous entry endpoint %d times", got)
		}
		if got := f.callCount(fixtureFolderRoute(fixtureRealRootID)); got != 1 {
			t.Fatalf("realname root list calls=%d want=1", got)
		}
		if got := f.callCount("/api/efast/v1/file/metadata"); got != 1 {
			t.Fatalf("realname metadata calls=%d want=1", got)
		}
		if got := f.callCount("/api/efast/v1/file/osdownload"); got != 1 {
			t.Fatalf("realname download authorization calls=%d want=1", got)
		}
		if got := f.callCount("object:/download"); got != 1 {
			t.Fatalf("realname object calls=%d want=1", got)
		}
	})

	for _, tc := range []struct {
		name, linkID string
	}{
		{name: "password-protected public link", linkID: fixturePasswordLinkID},
		{name: "mobile-verified public link", linkID: fixtureMobileLinkID},
	} {
		t.Run(tc.name+" stops before documents", func(t *testing.T) {
			f := newFixture(t)
			r := newRuntime(t, f)
			link := f.control.URL + "/link/" + tc.linkID
			result := runJSON(t, r, []string{"get", "--share", link, t.TempDir(), "top.txt", "--json"}, 3)
			if result["ok"] != false || result["category"] != "auth" {
				t.Fatalf("restricted share result=%v", result)
			}
			if got := f.callCountPrefix("/api/efast/"); got != 0 {
				t.Fatalf("restricted share sent %d document requests", got)
			}
			if got := f.callCountPrefix("object:"); got != 0 {
				t.Fatalf("restricted share sent %d object requests", got)
			}
		})
	}

	t.Run("other origin is rejected before network", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		result := runJSON(t, r, []string{"ls", "--share", "https://other.invalid/link/FakeOtherHostMarker", "--json"}, 2)
		if result["ok"] != false || result["category"] != "usage" {
			t.Fatalf("other-origin result=%v", result)
		}
		if got := f.callCountPrefix("/link/"); got != 0 {
			t.Fatalf("other-origin link made %d control requests", got)
		}
	})

	t.Run("cross-origin redirect is not followed", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		link := f.control.URL + "/link/" + fixtureRedirectLinkID
		result := runJSON(t, r, []string{"ls", "--share", link, "--json"}, 5)
		if result["ok"] != false || result["category"] != "network" {
			t.Fatalf("cross-origin redirect result=%v", result)
		}
		if got := f.callCount("object:/redirect-target"); got != 0 {
			t.Fatalf("cross-origin redirect target received %d requests", got)
		}
		if got := f.callCount("/api/shared-link/v1/links/" + fixtureRedirectLinkID); got != 1 {
			t.Fatalf("cross-origin redirect metadata calls=%d want=1", got)
		}
		if got := f.callCountPrefix("/api/efast/"); got != 0 {
			t.Fatalf("cross-origin redirect continued with %d document requests", got)
		}
	})
}

func fixtureFolderRoute(id string) string {
	return "/api/efast/v1/folders/" + id + "/sub_objects"
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

func assertListingIncludes(t *testing.T, listing map[string]any, path string) {
	t.Helper()
	for _, raw := range listing["entries"].([]any) {
		if raw.(map[string]any)["remote_path"] == path {
			return
		}
	}
	t.Fatalf("listing does not include %q: %v", path, listing)
}

func assertListingExcludes(t *testing.T, listing map[string]any, path string) {
	t.Helper()
	for _, raw := range listing["entries"].([]any) {
		if raw.(map[string]any)["remote_path"] == path {
			t.Fatalf("listing still includes %q: %v", path, listing)
		}
	}
}

func TestCLIFailureRecovery(t *testing.T) {
	t.Run("401 refreshes once", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		loginCLI(t, r)
		beforeTokens := f.callCount("/oauth2/token")
		f.setFault("entry-401", 1)
		runJSON(t, r, []string{"ls", "/", "--json"}, 0)
		if got := f.callCount("/api/efast/v1/entry-doc-lib"); got != 2 {
			t.Fatalf("entry calls=%d want=2", got)
		}
		if got := f.callCount("/oauth2/token") - beforeTokens; got != 1 {
			t.Fatalf("refresh calls=%d want=1", got)
		}
	})
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		status := status
		t.Run("retry "+strconv.Itoa(status), func(t *testing.T) {
			f := newFixture(t)
			r := newRuntime(t, f)
			loginCLI(t, r)
			fault := "entry-429"
			if status == http.StatusServiceUnavailable {
				fault = "entry-503"
			}
			f.setFault(fault, 1)
			runJSON(t, r, []string{"ls", "/", "--json"}, 0)
			if got := f.callCount("/api/efast/v1/entry-doc-lib"); got != 2 {
				t.Fatalf("status %d entry calls=%d want=2", status, got)
			}
		})
	}
	t.Run("expired signed request refreshes", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		loginCLI(t, r)
		runJSON(t, r, []string{"mkdir", "/个人文档/recovery", "--parents", "--json"}, 0)
		local, payload := writePayload(t, f, "expired.bin", 1<<20)
		f.setFault("signed-expired", 1)
		result := runJSON(t, r, []string{"put", local, "/个人文档/recovery/expired.bin", "--json"}, 0)
		if result["remote_id"] == "" || f.callCount("object:/single") != 2 || f.callCount("/api/efast/v1/file/osuploadrefresh") != 1 || f.remainingFault("signed-expired") != 0 || !bytes.Equal(f.fileBytes("/个人文档/recovery/expired.bin"), payload) {
			t.Fatalf("expired recovery result=%v single=%d refresh=%d", result, f.callCount("object:/single"), f.callCount("/api/efast/v1/file/osuploadrefresh"))
		}
	})
	t.Run("dropped create response reconciles without duplicate", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		loginCLI(t, r)
		f.setFault("drop-create", 1)
		result := runJSON(t, r, []string{"mkdir", "/个人文档/dropped", "--json"}, 0)
		if result["remote_id"] == "" || f.callCount("/api/efast/v1/dir/create") != 1 || f.remainingFault("drop-create") != 0 || f.childCount("gns://library", "dropped") != 1 {
			t.Fatalf("create result=%v calls=%d duplicates=%d", result, f.callCount("/api/efast/v1/dir/create"), f.childCount("gns://library", "dropped"))
		}
	})
	t.Run("dropped finish response reconciles without duplicate", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		loginCLI(t, r)
		runJSON(t, r, []string{"mkdir", "/个人文档/recovery", "--json"}, 0)
		local, payload := writePayload(t, f, "finish.bin", 1<<20)
		f.setFault("drop-finish", 1)
		result := runJSON(t, r, []string{"put", local, "/个人文档/recovery/finish.bin", "--json"}, 0)
		if result["remote_id"] == "" || f.callCount("/api/efast/v1/file/osendupload") != 1 || f.remainingFault("drop-finish") != 0 || f.childCountByPath("/个人文档/recovery/finish.bin") != 1 || !bytes.Equal(f.fileBytes("/个人文档/recovery/finish.bin"), payload) {
			t.Fatalf("finish result=%v finish-calls=%d", result, f.callCount("/api/efast/v1/file/osendupload"))
		}
	})
	t.Run("mismatched metadata is integrity exit 7", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		loginCLI(t, r)
		runJSON(t, r, []string{"mkdir", "/个人文档/recovery", "--json"}, 0)
		local, _ := writePayload(t, f, "mismatch.bin", 1<<20)
		f.setFault("metadata-mismatch", 1)
		result := runJSON(t, r, []string{"put", local, "/个人文档/recovery/mismatch.bin", "--json"}, 7)
		if result["ok"] != false || result["category"] != "integrity" {
			t.Fatalf("integrity result=%v", result)
		}
	})
	t.Run("cancel after two saved parts and resume complement", func(t *testing.T) {
		f := newFixture(t)
		r := newRuntime(t, f)
		loginCLI(t, r)
		runJSON(t, r, []string{"mkdir", "/个人文档/recovery", "--json"}, 0)
		local, payload := writePayload(t, f, "resume.bin", 20<<20)
		f.mu.Lock()
		f.gateTwo = true
		f.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		outcomeCh := make(chan cliOutcome, 1)
		go func() {
			var stdout, stderr bytes.Buffer
			code := cli.Run(ctx, []string{"put", local, "/个人文档/recovery/resume.bin", "--json"}, strings.NewReader(""), &stdout, &stderr, r.deps, version.Info{})
			outcomeCh <- cliOutcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
		}()
		waitForCompletedParts(t, r.paths.UploadStateDir, 2)
		cancel()
		outcome, ok := awaitCLIOutcome(outcomeCh, 10*time.Second)
		if !ok {
			t.Fatal("canceled CLI did not return within 10 seconds")
		}
		if outcome.code != 130 {
			t.Fatalf("cancel exit=%d stdout=%q stderr=%q", outcome.code, outcome.stdout, outcome.stderr)
		}
		completed := f.completedParts()
		if len(completed) != 2 || countStateJSON(t, r.paths.UploadStateDir) != 1 {
			t.Fatalf("completed=%v state-json=%d", completed, countStateJSON(t, r.paths.UploadStateDir))
		}
		state := loadOnlyState(t, r.paths.UploadStateDir, r.objectURL)
		if len(state.Completed) != 2 {
			t.Fatalf("saved completed parts=%v", state.Completed)
		}
		f.mu.Lock()
		f.gateTwo = false
		before := cloneIntMap(f.partTry)
		f.mu.Unlock()
		result := runJSON(t, r, []string{"put", local, "/个人文档/recovery/resume.bin", "--json"}, 0)
		if result["resumed"] != true || int(result["parts_resumed"].(float64)) != 2 || int(result["parts_uploaded"].(float64)) != 3 || countStateJSON(t, r.paths.UploadStateDir) != 0 || !bytes.Equal(f.fileBytes("/个人文档/recovery/resume.bin"), payload) {
			t.Fatalf("resume result=%v state-json=%d", result, countStateJSON(t, r.paths.UploadStateDir))
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for part := 1; part <= 5; part++ {
			delta := f.partTry[part] - before[part]
			_, wasComplete := completed[part]
			// Signed PUT is intentionally transport-retryable. A missing part may
			// therefore have more than one network attempt, but a saved part must
			// never be sent again.
			if (wasComplete && delta != 0) || (!wasComplete && delta < 1) {
				t.Fatalf("part %d completed-before=%v rerun-attempts=%d", part, wasComplete, delta)
			}
		}
	})
}

func writePayload(t *testing.T, f *fixture, name string, size int) (string, []byte) {
	t.Helper()
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte((i*17 + len(name)) % 251)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	f.expectFile(t, path, name, payload, false)
	return path, payload
}

func (f *fixture) expectFile(t *testing.T, path, name string, data []byte, instant bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f.registerExpected(contentEvidence(name, data, info.ModTime().UnixNano()/1000, instant))
}

func (f *fixture) registerExpected(expected expectedContent) {
	f.mu.Lock()
	f.expectedByName[expected.Name] = expected
	f.expectedBySlice[expected.SliceMD5] = expected
	if expected.Instant {
		f.instant[expected.Size] = append([]byte(nil), expected.Data...)
	}
	f.mu.Unlock()
}

func contentEvidence(name string, data []byte, clientMtimeUS int64, instant bool) expectedContent {
	full := md5.Sum(data)
	sliceEnd := len(data)
	if sliceEnd > 200*1024 {
		sliceEnd = 200 * 1024
	}
	slice := md5.Sum(data[:sliceEnd])
	return expectedContent{
		Name: name, Data: append([]byte(nil), data...), Size: int64(len(data)), ClientMtimeUS: clientMtimeUS,
		MD5: strings.ToUpper(hex.EncodeToString(full[:])), SliceMD5: strings.ToUpper(hex.EncodeToString(slice[:])),
		CRC32: fmt.Sprintf("%08X", crc32.ChecksumIEEE(data)), Instant: instant,
	}
}

func (f *fixture) setFault(name string, count int) {
	f.mu.Lock()
	f.faults[name] = count
	f.mu.Unlock()
}

func (f *fixture) callCount(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[route]
}

func (f *fixture) callCountPrefix(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for route, count := range f.calls {
		if strings.HasPrefix(route, prefix) {
			total += count
		}
	}
	return total
}

func (f *fixture) remainingFault(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.faults[name]
}

func (f *fixture) childCount(parentID, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, node := range f.nodes {
		if node.ParentID == parentID && node.Name == name {
			count++
		}
	}
	return count
}

func (f *fixture) childCountByPath(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, node := range f.nodes {
		if node.Path == path {
			count++
		}
	}
	return count
}

func (f *fixture) completedParts() map[int]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneBoolMap(f.partOK)
}

func cloneBoolMap(source map[int]bool) map[int]bool {
	result := make(map[int]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneIntMap(source map[int]int) map[int]int {
	result := make(map[int]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func awaitCLIOutcome(outcomes <-chan cliOutcome, timeout time.Duration) (cliOutcome, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case outcome := <-outcomes:
		return outcome, true
	case <-timer.C:
		return cliOutcome{}, false
	}
}

func waitForCompletedParts(t *testing.T, dir string, want int) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			var state upload.State
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err == nil && json.Unmarshal(data, &state) == nil && len(state.Completed) == want {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("resume state never reached %d completed parts", want)
		case <-ticker.C:
		}
	}
}

func countStateJSON(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			count++
		}
	}
	return count
}

func loadOnlyState(t *testing.T, dir, objectURL string) upload.State {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("resume state mode=%#o", info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if containsSecretMaterial(data, objectURL) {
			t.Fatalf("resume state contains secret material: %s", data)
		}
		var state upload.State
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	t.Fatal("resume state JSON not found")
	return upload.State{}
}

func containsSecretMaterial(data []byte, objectURL string) bool {
	lower := strings.ToLower(string(data))
	markers := []string{
		fixtureAccess, fixtureRefreshedAccess, fixtureRefresh, fixtureSecret, objectURL,
		fixtureShareToken, fixturePublicLinkID, fixtureRealnameLinkID, fixturePasswordLinkID,
		fixtureMobileLinkID, fixtureRedirectLinkID,
		"authorization", "cookie", "signature=", "aws fixture", "fixture-single-generation-",
		"fixture-part-", "fixture-complete-",
	}
	for _, marker := range markers {
		if marker != "" && strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func loginCLI(t *testing.T, r testRuntime) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"login"}, strings.NewReader(""), &stdout, &stderr, r.deps, version.Info{}); code != 0 {
		t.Fatalf("login exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func runJSON(t *testing.T, r testRuntime, args []string, wantExit int) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, r.deps, version.Info{})
	if code != wantExit {
		t.Fatalf("%v exit=%d want=%d stdout=%q stderr=%q", args, code, wantExit, stdout.String(), stderr.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("%v decode stdout: %v; stdout=%q", args, err, stdout.String())
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		t.Fatalf("%v stdout is not exactly one JSON object: %q", args, stdout.String())
	}
	combined := stdout.String() + stderr.String()
	for _, secret := range []string{
		fixtureAccess, fixtureRefresh, fixtureSecret, fixtureShareToken, fixturePublicLinkID,
		fixtureRealnameLinkID, fixturePasswordLinkID, fixtureMobileLinkID, fixtureRedirectLinkID,
		r.objectURL, "signature=", "AWS fixture",
	} {
		if strings.Contains(combined, secret) {
			t.Fatalf("%v leaked %q: stdout=%q stderr=%q", args, secret, stdout.String(), stderr.String())
		}
	}
	lower := strings.ToLower(combined)
	for _, header := range []string{"authorization", "cookie"} {
		if strings.Contains(lower, header) {
			t.Fatalf("%v leaked credential header %q: stdout=%q stderr=%q", args, header, stdout.String(), stderr.String())
		}
	}
	return result
}

func (f *fixture) objectPutCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts[name]
}

func (f *fixture) fileBytes(path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.files[path]...)
}

type callbackBrowser struct {
	started chan struct{}
	done    chan error
	once    sync.Once
}

func (b *callbackBrowser) Open(ctx context.Context, rawURL string) error {
	authorizationURL, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	callbackURL := authorizationURL.Query().Get("redirect_uri")
	state := authorizationURL.Query().Get("state")
	b.once.Do(func() { close(b.started) })
	go func() {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, callbackURL+"?code=fixture-code&state="+url.QueryEscape(state), nil)
		if err != nil {
			b.done <- err
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			b.done <- err
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		b.done <- response.Body.Close()
	}()
	return nil
}

type testRuntime struct {
	deps      cli.Dependencies
	paths     config.Paths
	objectURL string
}

type fixtureShareSession struct{ session *sharelink.Session }

func (s fixtureShareSession) Root() cli.ShareEntry {
	return fixtureShareEntry(s.session.Root())
}

func (s fixtureShareSession) List(ctx context.Context, relativeDir string) ([]cli.ShareEntry, error) {
	entries, err := s.session.List(ctx, relativeDir)
	if err != nil {
		return nil, err
	}
	result := make([]cli.ShareEntry, len(entries))
	for index, entry := range entries {
		result[index] = fixtureShareEntry(entry)
	}
	return result, nil
}

func (s fixtureShareSession) Download(ctx context.Context, relativeFiles []string, localDir string, overwrite bool, progress upload.Progress) ([]cli.ShareGetResult, error) {
	downloads, err := s.session.Download(ctx, relativeFiles, localDir, overwrite, progress)
	if err != nil {
		return nil, err
	}
	result := make([]cli.ShareGetResult, len(downloads))
	for index, item := range downloads {
		result[index] = cli.ShareGetResult{
			SharePath: item.Entry.Path, Revision: item.Result.Revision,
			LocalPath: item.Result.LocalPath, Size: item.Result.Size,
		}
	}
	return result, nil
}

func fixtureShareEntry(entry sharelink.Entry) cli.ShareEntry {
	return cli.ShareEntry{Name: entry.Name, SharePath: entry.Path, Type: entry.Type, Size: entry.Size}
}

func newRuntime(t *testing.T, f *fixture) testRuntime {
	t.Helper()
	root := t.TempDir()
	paths := config.Paths{
		ConfigDir:       filepath.Join(root, "config", "pku-drive-cli"),
		ConfigFile:      filepath.Join(root, "config", "pku-drive-cli", "config.json"),
		CredentialsFile: filepath.Join(root, "config", "pku-drive-cli", "credentials.json"),
		CredentialLock:  filepath.Join(root, "config", "pku-drive-cli", "credentials.lock"),
		UploadStateDir:  filepath.Join(root, "state", "pku-drive-cli", "uploads"),
	}
	store := config.NewStore(paths)
	if err := store.SaveConfig(config.Config{Server: f.control.URL}); err != nil {
		t.Fatalf("save fixture config: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(f.control.Certificate())
	roots.AddCert(f.objects.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.RootCAs = roots
	httpClient := httpx.New(transport, httpx.Policy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond})
	oauthClient := oauth.NewClient(f.control.URL, httpClient)
	browser := &callbackBrowser{started: make(chan struct{}), done: make(chan error, 1)}
	t.Cleanup(func() {
		select {
		case <-browser.started:
			select {
			case err := <-browser.done:
				if err != nil {
					t.Errorf("browser callback: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("browser callback did not finish")
			}
		default:
		}
	})
	authorizer := oauth.NewBrowserAuthorizer(oauthClient, browser)
	authorizer.Random = bytes.NewReader(bytes.Repeat([]byte{0x42}, 64))
	authorizer.CallbackTimeout = 5 * time.Second
	manager := &auth.Manager{Store: store, OAuth: oauthClient, Authorizer: authorizer, Now: func() time.Time { return fixtureNow }}
	api := anyshare.NewClient(f.control.URL, httpClient, manager)
	shareOpener, err := sharelink.NewOpener(f.control.URL, httpClient, api)
	if err != nil {
		t.Fatalf("construct shared-link opener: %v", err)
	}
	remoteService := remote.NewService(api)
	uploader := upload.NewUploader(f.control.URL, api, remoteService, upload.NewStateStore(paths.UploadStateDir), 4)
	uploader.Now = func() time.Time { return fixtureNow }
	downloader := download.NewDownloader(api)
	deps := cli.Dependencies{
		Login: func(ctx context.Context, callbackInput io.Reader, notice io.Writer, pasteCallback bool) error {
			if !pasteCallback {
				callbackInput = nil
			}
			_, err := manager.Login(ctx, callbackInput, notice)
			return err
		},
		Status: func(ctx context.Context) (cli.StatusResult, error) {
			status, err := manager.Status(ctx)
			if err != nil {
				return cli.StatusResult{}, err
			}
			if !status.LoggedIn {
				return cli.StatusResult{}, apperr.Wrap(apperr.Auth, "status", "login required", os.ErrNotExist)
			}
			user, err := api.CurrentUser(ctx)
			if err != nil {
				return cli.StatusResult{}, err
			}
			return cli.StatusResult{LoggedIn: true, Server: status.Server, Account: user.Account, ExpiresAt: &status.ExpiresAt}, nil
		},
		List: func(ctx context.Context, path string) ([]cli.ItemResult, error) {
			items, err := remoteService.List(ctx, path)
			if err != nil {
				return nil, err
			}
			results := make([]cli.ItemResult, len(items))
			for i, item := range items {
				results[i] = cli.ItemResult{Name: item.Name, RemotePath: item.Path, Type: item.Type, RemoteID: item.ID, Size: item.Size, Modified: item.Modified}
			}
			return results, nil
		},
		Mkdir: func(ctx context.Context, path string, parents bool) (cli.ItemResult, error) {
			item, err := remoteService.Mkdir(ctx, path, parents)
			return cli.ItemResult{Name: item.Name, RemotePath: item.Path, Type: item.Type, RemoteID: item.ID, Size: item.Size, Modified: item.Modified}, err
		},
		Put: func(ctx context.Context, localPath, remotePath string, overwrite bool, progress upload.Progress) (cli.PutResult, error) {
			target, err := remoteService.ResolveUploadTarget(ctx, filepath.Base(localPath), remotePath)
			if err != nil {
				return cli.PutResult{}, err
			}
			result, err := uploader.Put(ctx, localPath, target, overwrite, progress)
			return cli.PutResult{RemotePath: result.RemotePath, RemoteID: result.RemoteID, Revision: result.Revision, Size: result.Size, Resumed: result.Resumed, Instant: result.Instant, Multipart: result.Multipart, PartsTotal: result.PartsTotal, PartsResumed: result.PartsResumed, PartsUploaded: result.PartsUploaded}, err
		},
		Get: func(ctx context.Context, remotePath, localPath string, overwrite bool, progress upload.Progress) (cli.GetResult, error) {
			item, err := remoteService.Resolve(ctx, remotePath)
			if err != nil {
				return cli.GetResult{}, err
			}
			result, err := downloader.Get(ctx, item, localPath, overwrite, progress)
			if err != nil {
				return cli.GetResult{}, err
			}
			return cli.GetResult{RemotePath: item.Path, RemoteID: result.RemoteID, Revision: result.Revision, LocalPath: result.LocalPath, Size: result.Size}, nil
		},
		OpenShare: func(ctx context.Context, rawLink string) (cli.ShareSession, error) {
			session, err := shareOpener.Open(ctx, rawLink)
			if err != nil {
				return nil, err
			}
			return fixtureShareSession{session: session}, nil
		},
		Delete: func(ctx context.Context, remotePath string, recursive bool) (cli.DeleteResult, error) {
			result, err := remoteService.Delete(ctx, remotePath, recursive)
			if err != nil {
				return cli.DeleteResult{}, err
			}
			return cli.DeleteResult{
				RemotePath: result.Item.Path, RemoteID: result.Item.ID, Type: result.Item.Type,
				Status: string(result.Status), PendingReview: result.Status == anyshare.DeleteStatusPendingReview,
			}, nil
		},
		Logout: manager.Logout,
	}
	return testRuntime{deps: deps, paths: paths, objectURL: f.objects.URL}
}
