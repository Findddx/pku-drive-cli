package upload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/remote"
)

type nopProgress struct{}

func (nopProgress) Started(int64)  {}
func (nopProgress) Advanced(int64) {}
func (nopProgress) Finished()      {}

type fakeUploadAPI struct {
	mu sync.Mutex

	limits anyshare.StorageOptions
	match  bool

	directCalls    int
	beginCalls     int
	initCalls      int
	finishCalls    int
	refreshes      int
	completeCalls  int
	beginRequests  []anyshare.BeginRequest
	finishRequests []anyshare.FinishRequest

	directLost         bool
	beginLost          bool
	finishLost         bool
	failPart           int
	expirePart         int
	expired            bool
	expireCompletion   bool
	completionExpired  bool
	refreshUploadID    string
	putCalls           []int
	authorizeUploadIDs []string
	singlePuts         int
	completionPuts     int

	committed   anyshare.Item
	fingerprint Fingerprint
	target      remote.UploadTarget

	partTwoStarted chan struct{}
	partTwoOnce    sync.Once
	releasePartTwo chan struct{}
}

func (f *fakeUploadAPI) StorageOptions(context.Context) (anyshare.StorageOptions, error) {
	return f.limits, nil
}

func (f *fakeUploadAPI) PreUpload(context.Context, int64, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.match, nil
}

func (f *fakeUploadAPI) DirectUpload(_ context.Context, req anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.directCalls++
	f.commit("doc-1", "rev-final")
	if f.directLost {
		return anyshare.UploadResult{}, networkFixture("lost direct response")
	}
	return anyshare.UploadResult{DocID: "doc-1", Rev: "rev-final", Name: req.Name}, nil
}

func (f *fakeUploadAPI) BeginSingle(_ context.Context, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginCalls++
	f.beginRequests = append(f.beginRequests, req)
	if f.beginLost {
		f.commit("doc-1", "rev-final")
		return anyshare.BeginUpload{}, networkFixture("lost begin response")
	}
	docID := "doc-1"
	if req.ExistingID != "" {
		docID = req.ExistingID
	}
	return anyshare.BeginUpload{DocID: docID, Rev: "rev-upload", Name: f.target.Name, Request: &anyshare.SignedRequest{Method: "PUT", URL: "memory://single"}}, nil
}

func (f *fakeUploadAPI) InitMultipart(_ context.Context, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initCalls++
	f.beginRequests = append(f.beginRequests, req)
	docID := "doc-1"
	if req.ExistingID != "" {
		docID = req.ExistingID
	}
	return anyshare.BeginUpload{DocID: docID, Rev: "rev-upload", Name: f.target.Name, UploadID: "upload-1"}, nil
}

func (f *fakeUploadAPI) AuthorizeParts(_ context.Context, _, _, uploadID string, first, last int) (anyshare.PartAuthorization, error) {
	f.mu.Lock()
	f.authorizeUploadIDs = append(f.authorizeUploadIDs, uploadID)
	f.mu.Unlock()
	result := make(anyshare.PartAuthorization, last-first+1)
	for part := first; part <= last; part++ {
		result[part] = anyshare.SignedRequest{Method: "PUT", URL: "memory://" + uploadID + "/part/" + strconv.Itoa(part)}
	}
	return result, nil
}

func (f *fakeUploadAPI) CompleteMultipart(context.Context, string, string, string, map[int]anyshare.PartInfo) (anyshare.SignedRequest, []byte, error) {
	f.mu.Lock()
	f.completeCalls++
	f.mu.Unlock()
	return anyshare.SignedRequest{Method: "PUT", URL: "memory://complete"}, []byte("<CompleteMultipartUpload/>"), nil
}

func (f *fakeUploadAPI) RefreshUpload(context.Context, string, string, int64, bool) (anyshare.RefreshResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	if f.refreshUploadID != "" {
		return anyshare.RefreshResult{UploadID: f.refreshUploadID}, nil
	}
	return anyshare.RefreshResult{Request: &anyshare.SignedRequest{Method: "PUT", URL: "memory://single-refresh"}}, nil
}

func (f *fakeUploadAPI) FinishUpload(_ context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishCalls++
	f.finishRequests = append(f.finishRequests, req)
	f.commit(req.DocID, req.Rev)
	if f.finishLost {
		return anyshare.UploadResult{}, networkFixture("lost finish response")
	}
	return anyshare.UploadResult{DocID: req.DocID, Rev: req.Rev, Name: f.target.Name}, nil
}

func (f *fakeUploadAPI) FileMetadata(context.Context, string, string) (anyshare.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.committed.ID == "" {
		return anyshare.Item{}, networkFixture("metadata absent")
	}
	return f.committed, nil
}

func (f *fakeUploadAPI) PutSigned(ctx context.Context, signed anyshare.SignedRequest, body io.Reader, _ func() (io.ReadCloser, error), length int64) (string, error) {
	if _, err := io.CopyN(io.Discard, body, length); err != nil {
		return "", err
	}
	if signed.URL == "memory://complete" {
		f.mu.Lock()
		if f.expireCompletion && !f.completionExpired {
			f.completionExpired = true
			f.mu.Unlock()
			return "", anyshare.ErrExpiredSignature
		}
		f.completionPuts++
		f.mu.Unlock()
		return "", nil
	}
	if signed.URL == "memory://single" || signed.URL == "memory://single-refresh" {
		f.mu.Lock()
		f.singlePuts++
		f.mu.Unlock()
		return `"single-etag"`, nil
	}
	part, err := strconv.Atoi(filepath.Base(signed.URL))
	if err != nil {
		return "", fmt.Errorf("bad synthetic signed URL")
	}
	f.mu.Lock()
	f.putCalls = append(f.putCalls, part)
	if part == f.expirePart && !f.expired {
		f.expired = true
		f.mu.Unlock()
		return "", anyshare.ErrExpiredSignature
	}
	fail := part == f.failPart
	partTwoStarted, releasePartTwo := f.partTwoStarted, f.releasePartTwo
	f.mu.Unlock()
	if part == 2 && releasePartTwo != nil {
		if partTwoStarted != nil {
			f.partTwoOnce.Do(func() { close(partTwoStarted) })
		}
		select {
		case <-releasePartTwo:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if fail {
		return "", networkFixture("fixture part failure")
	}
	return fmt.Sprintf(`"etag-%d"`, part), nil
}

func (f *fakeUploadAPI) commit(id, rev string) {
	f.committed = anyshare.Item{ID: id, DocID: id, Rev: rev, Name: f.target.Name, Path: f.target.RemotePath,
		Type: "file", Size: f.fingerprint.Size, ClientMtimeUS: f.fingerprint.MtimeNS / 1000,
		MD5: f.fingerprint.MD5, SliceMD5: f.fingerprint.SliceMD5, CRC32: f.fingerprint.CRC32}
}

func (f *fakeUploadAPI) ResetPutCalls() { f.mu.Lock(); f.putCalls = nil; f.mu.Unlock() }
func (f *fakeUploadAPI) PutCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.putCalls...)
}

type fakeRemoteVerifier struct {
	api      *fakeUploadAPI
	resolves int
}

func (r *fakeRemoteVerifier) Resolve(context.Context, string) (anyshare.Item, error) {
	r.api.mu.Lock()
	defer r.api.mu.Unlock()
	r.resolves++
	if r.api.committed.ID == "" {
		return anyshare.Item{}, networkFixture("not found")
	}
	return r.api.committed, nil
}

type uploaderHarness struct {
	localPath   string
	target      remote.UploadTarget
	api         *fakeUploadAPI
	remote      *fakeRemoteVerifier
	states      *StateStore
	stateKey    string
	uploader    *Uploader
	fingerprint Fingerprint
}

func newUploaderHarness(t *testing.T, size int64, limits Limits) *uploaderHarness {
	t.Helper()
	dir := t.TempDir()
	localPath := filepath.Join(dir, "fixture.bin")
	file, err := os.Create(localPath)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte("uploader-fixture-"), 4096)
	for remaining := size; remaining > 0; {
		chunk := int64(len(block))
		if chunk > remaining {
			chunk = remaining
		}
		if _, err := file.Write(block[:chunk]); err != nil {
			t.Fatal(err)
		}
		remaining -= chunk
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	descriptor, fingerprint, err := OpenFingerprint(localPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = descriptor.Close()
	target := remote.UploadTarget{RemotePath: "/Library/fixture.bin", ParentID: "parent-1", Name: "fixture.bin"}
	api := &fakeUploadAPI{limits: anyshare.StorageOptions{PartMinSize: limits.PartMinSize, PartMaxSize: limits.PartMaxSize, PartMaxNum: limits.PartMaxNum}, fingerprint: fingerprint, target: target}
	remoteFake := &fakeRemoteVerifier{api: api}
	states := NewStateStore(filepath.Join(dir, "state"))
	uploader := NewUploader("https://example.invalid", api, remoteFake, states, 1)
	uploader.sleep = func(context.Context, time.Duration) error { return nil }
	abs, err := filepath.Abs(localPath)
	if err != nil {
		t.Fatal(err)
	}
	return &uploaderHarness{localPath: localPath, target: target, api: api, remote: remoteFake, states: states,
		stateKey: StateKey("https://example.invalid", target.RemotePath, abs), uploader: uploader, fingerprint: fingerprint}
}

func TestUploaderInstantSingleConflictAndOverwrite(t *testing.T) {
	limits := Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100}
	t.Run("instant new file", func(t *testing.T) {
		// Production mutation caught: routing a positive new-file match through object PUT instead of DirectUpload.
		h := newUploaderHarness(t, 1024, limits)
		h.api.match = true
		result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Instant || result.RemoteID != "doc-1" || h.api.directCalls != 1 || h.api.singlePuts != 0 {
			t.Fatalf("result=%+v api=%+v", result, h.api)
		}
	})
	t.Run("single non-match", func(t *testing.T) {
		// Production mutation caught: skipping the sole signed PUT or final metadata verification for a small non-match.
		h := newUploaderHarness(t, 1024, limits)
		result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
		if err != nil {
			t.Fatal(err)
		}
		if result.PartsTotal != 1 || result.PartsUploaded != 1 || h.api.beginCalls != 1 || h.api.singlePuts != 1 || h.api.finishCalls != 1 || h.remote.resolves == 0 {
			t.Fatalf("result=%+v", result)
		}
	})
	t.Run("existing conflict", func(t *testing.T) {
		// Production mutation caught: making any upload API call before rejecting an existing target without overwrite.
		h := newUploaderHarness(t, 1024, limits)
		existing := anyshare.Item{ID: "old-doc", Rev: "old-rev", Type: "file"}
		h.target.Existing = &existing
		h.api.target = h.target
		_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
		assertUploadCategory(t, err, apperr.Remote)
		if h.api.directCalls+h.api.beginCalls+h.api.initCalls+h.api.finishCalls != 0 {
			t.Fatalf("upload API was called")
		}
	})
	t.Run("overwrite ignores instant match", func(t *testing.T) {
		// Production mutation caught: direct-uploading an overwrite or losing the original revision between begin and finish.
		h := newUploaderHarness(t, 1024, limits)
		h.api.match = true
		existing := anyshare.Item{ID: "old-doc", Rev: "old-rev", Type: "file"}
		h.target.Existing = &existing
		h.api.target = h.target
		result, err := h.uploader.Put(context.Background(), h.localPath, h.target, true, nopProgress{})
		if err != nil {
			t.Fatal(err)
		}
		if h.api.directCalls != 0 || result.RemoteID != "old-doc" {
			t.Fatalf("result=%+v", result)
		}
		if got := h.api.beginRequests[0]; got.ExistingID != "old-doc" || got.EditedRev != "old-rev" {
			t.Fatalf("begin=%+v", got)
		}
		if got := h.api.finishRequests[0]; got.EditedRev != "old-rev" {
			t.Fatalf("finish=%+v", got)
		}
	})
}

func TestUploaderReconcilesLostNonIdempotentResponses(t *testing.T) {
	limits := Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100}
	for _, test := range []struct {
		name      string
		configure func(*fakeUploadAPI)
	}{
		{"direct", func(api *fakeUploadAPI) { api.match = true; api.directLost = true }},
		{"begin", func(api *fakeUploadAPI) { api.beginLost = true }},
		{"finish", func(api *fakeUploadAPI) { api.finishLost = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Production mutation caught: blindly repeating an ambiguous non-idempotent write instead of reconciling exact remote metadata.
			h := newUploaderHarness(t, 1024, limits)
			test.configure(h.api)
			result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
			if err != nil {
				t.Fatal(err)
			}
			if result.RemoteID != "doc-1" || h.remote.resolves == 0 || h.api.directCalls > 1 || h.api.beginCalls > 1 || h.api.finishCalls > 1 {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestUploaderDoesNotReconcileDefiniteRemoteRejection(t *testing.T) {
	// Production mutation caught: treating every Remote-category rejection as ambiguous and adopting another actor's matching file.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	h.api.mu.Lock()
	h.api.commit("other-doc", "other-rev")
	h.api.mu.Unlock()
	h.uploader.API = &definiteDirectRejectionAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Remote)
	if h.remote.resolves != 0 {
		t.Fatalf("reconciliation resolves=%d", h.remote.resolves)
	}
}

type definiteDirectRejectionAPI struct{ UploadAPI }

func (d *definiteDirectRejectionAPI) DirectUpload(context.Context, anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	return anyshare.UploadResult{}, apperr.Wrap(apperr.Remote, "fixture", "precondition failed", errors.New("synthetic definite rejection"))
}

func TestUploaderReconcilesExplicitOutcomeUnknownMarker(t *testing.T) {
	// Production mutation caught: refusing reconciliation for a secret-free outcome-unknown marker carried by a Remote-category error.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	h.uploader.API = &markedDirectOutcomeAPI{UploadAPI: h.api, api: h.api}
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteID != "doc-1" || h.remote.resolves == 0 {
		t.Fatalf("result=%+v resolves=%d", result, h.remote.resolves)
	}
}

type markedDirectOutcomeAPI struct {
	UploadAPI
	api *fakeUploadAPI
}

func (m *markedDirectOutcomeAPI) DirectUpload(context.Context, anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	m.api.mu.Lock()
	m.api.directCalls++
	m.api.commit("doc-1", "rev-final")
	m.api.mu.Unlock()
	return anyshare.UploadResult{}, apperr.Wrap(apperr.Remote, "fixture", "unknown outcome", anyshare.ErrOutcomeUnknown)
}

func TestUploaderReconciliationUsesFiveExactContextAwareAttempts(t *testing.T) {
	// Production mutation caught: replaying a non-idempotent direct create, polling too many times, or using the wrong reconciliation delays.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	h.api.directLost = true
	h.uploader.Remote = &alwaysFailRemote{RemoteVerifier: h.remote}
	var delays []time.Duration
	h.uploader.sleep = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		delays = append(delays, delay)
		return nil
	}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Network)
	if h.api.directCalls != 1 || h.remote.resolves != 5 {
		t.Fatalf("direct=%d resolves=%d", h.api.directCalls, h.remote.resolves)
	}
	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("delays=%v want=%v", delays, want)
	}
}

func TestUploaderRetainsAttemptedPhaseAndNeverReplaysAcrossInvocations(t *testing.T) {
	limits := Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100}
	tests := []struct {
		name      string
		mode      string
		match     bool
		wantPhase OperationPhase
	}{
		{name: "direct", mode: "direct", match: true, wantPhase: PhaseDirectAttempted},
		{name: "begin", mode: "begin", wantPhase: PhaseBeginSingleAttempted},
		{name: "finish", mode: "finish", wantPhase: PhaseFinishAttempted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Production mutation caught: dropping an inconclusive attempted phase and replaying the same non-idempotent operation on a later Put.
			h := newUploaderHarness(t, 1024, limits)
			h.api.match = tt.match
			boundary := &inconclusiveOperationAPI{UploadAPI: h.api, mode: tt.mode}
			h.uploader.API = boundary
			h.uploader.Remote = &alwaysFailRemote{RemoteVerifier: h.remote}
			if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err == nil {
				t.Fatal("expected first inconclusive error")
			}
			saved, err := h.states.Load(h.stateKey)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Phase != tt.wantPhase {
				t.Fatalf("phase=%q want=%q", saved.Phase, tt.wantPhase)
			}
			before := boundary.nonIdempotentCalls()
			if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err == nil {
				t.Fatal("expected second inconclusive error")
			}
			if after := boundary.nonIdempotentCalls(); after != before {
				t.Fatalf("non-idempotent calls before=%d after=%d", before, after)
			}
			again, err := h.states.Load(h.stateKey)
			if err != nil || again.Phase != tt.wantPhase {
				t.Fatalf("retained state=%+v err=%v", again, err)
			}
		})
	}
}

func TestUploaderPersistsPhaseBeforeEachNonIdempotentCall(t *testing.T) {
	limits := Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100}
	for _, tt := range []struct {
		name, mode string
		match      bool
		phase      OperationPhase
	}{
		{name: "direct", mode: "direct", match: true, phase: PhaseDirectAttempted},
		{name: "begin", mode: "begin", phase: PhaseBeginSingleAttempted},
		{name: "finish", mode: "finish", phase: PhaseFinishAttempted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Production mutation caught: issuing a non-idempotent call before its attempted phase reaches the real StateStore.
			h := newUploaderHarness(t, 1024, limits)
			h.api.match = tt.match
			observer := &phaseObservingAPI{UploadAPI: h.api, states: h.states, key: h.stateKey, mode: tt.mode, want: tt.phase}
			h.uploader.API = observer
			if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err != nil {
				t.Fatal(err)
			}
			if !observer.observed {
				t.Fatal("non-idempotent boundary was not observed")
			}
		})
	}
}

type phaseObservingAPI struct {
	UploadAPI
	states    *StateStore
	key, mode string
	want      OperationPhase
	observed  bool
}

func (p *phaseObservingAPI) observe() error {
	state, err := p.states.Load(p.key)
	if err != nil {
		return err
	}
	if state.Phase != p.want {
		return fmt.Errorf("phase=%q want=%q", state.Phase, p.want)
	}
	p.observed = true
	return nil
}
func (p *phaseObservingAPI) DirectUpload(ctx context.Context, req anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	if p.mode == "direct" {
		if err := p.observe(); err != nil {
			return anyshare.UploadResult{}, err
		}
	}
	return p.UploadAPI.DirectUpload(ctx, req)
}
func (p *phaseObservingAPI) BeginSingle(ctx context.Context, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	if p.mode == "begin" {
		if err := p.observe(); err != nil {
			return anyshare.BeginUpload{}, err
		}
	}
	return p.UploadAPI.BeginSingle(ctx, req)
}
func (p *phaseObservingAPI) FinishUpload(ctx context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	if p.mode == "finish" {
		if err := p.observe(); err != nil {
			return anyshare.UploadResult{}, err
		}
	}
	return p.UploadAPI.FinishUpload(ctx, req)
}

func TestUploaderReconcilesPendingDirectBeforeNewExistingConflict(t *testing.T) {
	// Production mutation caught: rejecting the newly visible destination as a conflict before reconciling a persisted direct-attempted phase.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	boundary := &inconclusiveOperationAPI{UploadAPI: h.api, mode: "direct"}
	h.uploader.API = boundary
	h.uploader.Remote = &alwaysFailRemote{RemoteVerifier: h.remote}
	if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err == nil {
		t.Fatal("expected first inconclusive error")
	}
	before := boundary.nonIdempotentCalls()
	h.api.mu.Lock()
	h.api.commit("doc-1", "rev-final")
	committed := h.api.committed
	h.api.mu.Unlock()
	h.target.Existing = &committed
	h.uploader.Remote = h.remote
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteID != "doc-1" || boundary.nonIdempotentCalls() != before {
		t.Fatalf("result=%+v calls=%d before=%d", result, boundary.nonIdempotentCalls(), before)
	}
}

func TestUploaderRetainsAttemptedPhaseWhenLocalFingerprintChanges(t *testing.T) {
	// Production mutation caught: deleting an attempted phase and replaying its write merely because the current local fingerprint changed.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	boundary := &inconclusiveOperationAPI{UploadAPI: h.api, mode: "direct"}
	h.uploader.API = boundary
	h.uploader.Remote = &alwaysFailRemote{RemoteVerifier: h.remote}
	if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err == nil {
		t.Fatal("expected first inconclusive error")
	}
	before := boundary.nonIdempotentCalls()
	if err := os.Truncate(h.localPath, 7); err != nil {
		t.Fatal(err)
	}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
	if boundary.nonIdempotentCalls() != before {
		t.Fatalf("write replayed: before=%d after=%d", before, boundary.nonIdempotentCalls())
	}
	saved, loadErr := h.states.Load(h.stateKey)
	if loadErr != nil || saved.Phase != PhaseDirectAttempted || saved.Fingerprint.Size != 1024 {
		t.Fatalf("state=%+v err=%v", saved, loadErr)
	}
}

func TestUploaderRetainsAttemptedPhaseOnInterruptedWrite(t *testing.T) {
	// Production mutation caught: clearing an attempted phase after cancellation even though the in-flight write may have committed remotely.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	h.uploader.API = &interruptedDirectAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Interrupted)
	saved, loadErr := h.states.Load(h.stateKey)
	if loadErr != nil || saved.Phase != PhaseDirectAttempted {
		t.Fatalf("state=%+v err=%v", saved, loadErr)
	}
}

func TestUploaderClearsAttemptedPhaseWhenRequestWasNotSent(t *testing.T) {
	// Production mutation caught: treating a token/preflight failure known not to have dispatched as ambiguous and retaining a replay-blocking attempted phase.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	h.uploader.API = &requestNotSentDirectAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Network)
	if _, loadErr := h.states.Load(h.stateKey); !errors.Is(loadErr, os.ErrNotExist) {
		t.Fatalf("attempted state retained after request-not-sent failure: %v", loadErr)
	}
	if h.remote.resolves != 0 {
		t.Fatalf("reconciliation attempted after request-not-sent failure: resolves=%d", h.remote.resolves)
	}
}

type requestNotSentDirectAPI struct{ UploadAPI }

func (a *requestNotSentDirectAPI) DirectUpload(context.Context, anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	return anyshare.UploadResult{}, apperr.Wrap(apperr.Network, "fixture", "token unavailable", anyshare.ErrRequestNotSent)
}

func TestPendingReconciliationCancellationRemainsInterrupted(t *testing.T) {
	// Production mutation caught: converting cancellation during read-only pending reconciliation into a generic Network error.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	h.uploader.API = &inconclusiveOperationAPI{UploadAPI: h.api, mode: "direct"}
	h.uploader.Remote = &alwaysFailRemote{RemoteVerifier: h.remote}
	if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err == nil {
		t.Fatal("expected first inconclusive error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.uploader.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	_, err := h.uploader.Put(ctx, h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Interrupted)
	saved, loadErr := h.states.Load(h.stateKey)
	if loadErr != nil || saved.Phase != PhaseDirectAttempted {
		t.Fatalf("state=%+v err=%v", saved, loadErr)
	}
}

func TestInitialAmbiguousReconciliationCancellationRemainsInterrupted(t *testing.T) {
	// Production mutation caught: masking cancellation during first-invocation reconciliation with the earlier ambiguous write error.
	tests := []struct {
		name      string
		mode      string
		match     bool
		wantPhase OperationPhase
	}{
		{name: "direct", mode: "direct", match: true, wantPhase: PhaseDirectAttempted},
		{name: "begin", mode: "begin", wantPhase: PhaseBeginSingleAttempted},
		{name: "finish", mode: "finish", wantPhase: PhaseFinishAttempted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
			h.api.match = tt.match
			h.uploader.API = &inconclusiveOperationAPI{UploadAPI: h.api, mode: tt.mode}
			h.uploader.Remote = &alwaysFailRemote{RemoteVerifier: h.remote}
			h.uploader.sleep = func(context.Context, time.Duration) error { return context.Canceled }
			_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
			assertUploadCategory(t, err, apperr.Interrupted)
			saved, loadErr := h.states.Load(h.stateKey)
			if loadErr != nil || saved.Phase != tt.wantPhase {
				t.Fatalf("state=%+v err=%v", saved, loadErr)
			}
		})
	}
}

type interruptedDirectAPI struct{ UploadAPI }

func (i *interruptedDirectAPI) DirectUpload(context.Context, anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	return anyshare.UploadResult{}, apperr.Wrap(apperr.Interrupted, "fixture", "interrupted", context.Canceled)
}

type inconclusiveOperationAPI struct {
	UploadAPI
	mode                                 string
	directCalls, beginCalls, finishCalls int
}

func (a *inconclusiveOperationAPI) DirectUpload(ctx context.Context, req anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	if a.mode != "direct" {
		return a.UploadAPI.DirectUpload(ctx, req)
	}
	a.directCalls++
	return anyshare.UploadResult{}, networkFixture("inconclusive direct")
}
func (a *inconclusiveOperationAPI) BeginSingle(ctx context.Context, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	if a.mode != "begin" {
		return a.UploadAPI.BeginSingle(ctx, req)
	}
	a.beginCalls++
	return anyshare.BeginUpload{}, networkFixture("inconclusive begin")
}
func (a *inconclusiveOperationAPI) FinishUpload(ctx context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	if a.mode != "finish" {
		return a.UploadAPI.FinishUpload(ctx, req)
	}
	a.finishCalls++
	return anyshare.UploadResult{}, networkFixture("inconclusive finish")
}
func (a *inconclusiveOperationAPI) nonIdempotentCalls() int {
	return a.directCalls + a.beginCalls + a.finishCalls
}

type alwaysFailRemote struct{ RemoteVerifier }

func (r *alwaysFailRemote) Resolve(ctx context.Context, path string) (anyshare.Item, error) {
	_, _ = r.RemoteVerifier.Resolve(ctx, path)
	return anyshare.Item{}, networkFixture("resolve unavailable")
}

func TestMultipartResumesOnlyMissingParts(t *testing.T) {
	// Production mutation caught: failing to durably retain successful parts or re-uploading them on resume.
	h := newUploaderHarness(t, 20*1024*1024, Limits{PartMinSize: 5 * 1024 * 1024, PartMaxSize: 5 * 1024 * 1024, PartMaxNum: 100})
	h.api.failPart = 3
	if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err == nil {
		t.Fatal("expected first-run part failure")
	}
	saved, err := h.states.Load(h.stateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Completed[1]; !ok {
		t.Fatal("part 1 not saved")
	}
	if _, ok := saved.Completed[2]; !ok {
		t.Fatal("part 2 not saved")
	}
	if _, ok := saved.Completed[3]; ok {
		t.Fatal("failed part saved")
	}
	h.api.failPart = 0
	h.api.ResetPutCalls()
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resumed || result.PartsResumed != 2 {
		t.Fatalf("result=%+v", result)
	}
	if got := h.api.PutCalls(); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Fatalf("parts=%v", got)
	}
	if _, err := h.states.Load(h.stateKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state err=%v", err)
	}
}

func TestMultipartAuthorizesPartsIncrementallyBeforeSignedRequestsExpire(t *testing.T) {
	// A signed part request remains valid for only one later authorization. A
	// serial pre-authorization pass expires the first request before any worker
	// can use it; an unbuffered producer/worker pipeline keeps that lag bounded.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.Concurrency = 1
	expiring := &expiringAuthorizationAPI{
		UploadAPI:       h.api,
		maxLaterAllowed: 1,
		issuedAt:        make(map[string]int),
	}
	h.uploader.API = expiring

	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatalf("incremental multipart upload failed: %v", err)
	}
	if result.PartsUploaded != 4 || expiring.expired != 0 {
		t.Fatalf("result=%+v expired signed requests=%d", result, expiring.expired)
	}
	if expiring.maxLaterObserved > expiring.maxLaterAllowed {
		t.Fatalf("authorization lag=%d limit=%d", expiring.maxLaterObserved, expiring.maxLaterAllowed)
	}
}

type expiringAuthorizationAPI struct {
	UploadAPI
	mu               sync.Mutex
	authorizations   int
	maxLaterAllowed  int
	maxLaterObserved int
	expired          int
	issuedAt         map[string]int
}

func (a *expiringAuthorizationAPI) AuthorizeParts(ctx context.Context, docID, rev, uploadID string, first, last int) (anyshare.PartAuthorization, error) {
	auth, err := a.UploadAPI.AuthorizeParts(ctx, docID, rev, uploadID, first, last)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.authorizations++
	issuedAt := a.authorizations
	for _, signed := range auth {
		a.issuedAt[signed.URL] = issuedAt
	}
	a.mu.Unlock()
	return auth, nil
}

func (a *expiringAuthorizationAPI) PutSigned(ctx context.Context, signed anyshare.SignedRequest, body io.Reader, reopen func() (io.ReadCloser, error), length int64) (string, error) {
	a.mu.Lock()
	issuedAt, isPart := a.issuedAt[signed.URL]
	later := a.authorizations - issuedAt
	if isPart && later > a.maxLaterObserved {
		a.maxLaterObserved = later
	}
	if isPart && later > a.maxLaterAllowed {
		a.expired++
		a.mu.Unlock()
		return "", anyshare.ErrExpiredSignature
	}
	a.mu.Unlock()
	return a.UploadAPI.PutSigned(ctx, signed, body, reopen, length)
}

func TestMultipartAuthorizationErrorPropagatesFromIncrementalProducer(t *testing.T) {
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.API = &failingAuthorizationAPI{UploadAPI: h.api, failPart: 2}

	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Network)
	if !strings.Contains(err.Error(), "authorize upload part") {
		t.Fatalf("error does not identify authorization: %v", err)
	}
	if h.api.refreshes != 0 || h.api.finishCalls != 0 {
		t.Fatalf("refreshes=%d finish calls=%d", h.api.refreshes, h.api.finishCalls)
	}
}

type failingAuthorizationAPI struct {
	UploadAPI
	failPart int
}

func (a *failingAuthorizationAPI) AuthorizeParts(ctx context.Context, docID, rev, uploadID string, first, last int) (anyshare.PartAuthorization, error) {
	if first == a.failPart {
		return nil, networkFixture("fixture authorization failure")
	}
	return a.UploadAPI.AuthorizeParts(ctx, docID, rev, uploadID, first, last)
}

func TestMultipartPersistsEachETagAndRefreshesChangedUpload(t *testing.T) {
	// Production mutation caught: batching ETag state writes or retaining old-upload ETags after refresh changes UploadID.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.expirePart = 2
	h.api.refreshUploadID = "upload-2"
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if h.api.refreshes != 1 || result.PartsUploaded != 4 {
		t.Fatalf("refreshes=%d result=%+v puts=%v", h.api.refreshes, result, h.api.PutCalls())
	}
	got := h.api.PutCalls()
	sort.Ints(got)
	if !reflect.DeepEqual(got, []int{1, 1, 2, 2, 3, 4}) {
		t.Fatalf("parts=%v", got)
	}
	if h.api.completionPuts != 1 || h.api.finishCalls != 1 {
		t.Fatalf("completion=%d finish=%d", h.api.completionPuts, h.api.finishCalls)
	}
	h.api.mu.Lock()
	authorizedIDs := append([]string(nil), h.api.authorizeUploadIDs...)
	h.api.mu.Unlock()
	oldCount := 0
	for oldCount < len(authorizedIDs) && authorizedIDs[oldCount] == "upload-1" {
		oldCount++
	}
	if oldCount < 2 || oldCount > 3 || len(authorizedIDs)-oldCount != 4 {
		t.Fatalf("authorization upload IDs=%v", authorizedIDs)
	}
	for _, uploadID := range authorizedIDs[oldCount:] {
		if uploadID != "upload-2" {
			t.Fatalf("authorization upload IDs=%v", authorizedIDs)
		}
	}
}

func TestMultipartRefreshesPartAndLaterCompletionExpiry(t *testing.T) {
	// Production mutation caught: using one refresh allowance for independent part and multipart-completion signed requests.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.expirePart = 2
	h.api.expireCompletion = true
	h.api.refreshUploadID = "upload-2"
	if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err != nil {
		t.Fatal(err)
	}
	if h.api.refreshes != 2 || h.api.completeCalls != 2 || h.api.completionPuts != 1 {
		t.Fatalf("refreshes=%d completion calls=%d successful completion puts=%d", h.api.refreshes, h.api.completeCalls, h.api.completionPuts)
	}
}

func TestMultipartSavesETagBeforeAnotherPartCompletes(t *testing.T) {
	// Production mutation caught: deferring successful ETag persistence until the multipart worker batch finishes.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.Concurrency = 2
	h.api.partTwoStarted = make(chan struct{})
	h.api.releasePartTwo = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
		done <- err
	}()
	<-h.api.partTwoStarted
	for {
		state, err := h.states.Load(h.stateKey)
		if err == nil {
			if _, ok := state.Completed[1]; ok {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("upload ended before barrier: %v", err)
		default:
			runtime.Gosched()
		}
	}
	close(h.api.releasePartTwo)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUploaderCancellationAndLocalMutationPreserveState(t *testing.T) {
	limits := Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100}
	t.Run("cancellation", func(t *testing.T) {
		// Production mutation caught: scheduling parts after cancellation or deleting resumable state on interruption.
		h := newUploaderHarness(t, 20<<20, limits)
		ctx, cancel := context.WithCancel(context.Background())
		_, err := h.uploader.Put(ctx, h.localPath, h.target, false, &cancelAfterAdvance{cancel: cancel})
		assertUploadCategory(t, err, apperr.Interrupted)
		state, loadErr := h.states.Load(h.stateKey)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if _, ok := state.Completed[1]; !ok {
			t.Fatal("completed part lost")
		}
		if calls := h.api.PutCalls(); !reflect.DeepEqual(calls, []int{1}) {
			t.Fatalf("calls after cancel=%v", calls)
		}
	})
	t.Run("descriptor mutation", func(t *testing.T) {
		// Production mutation caught: returning success and removing state after the open local descriptor changes.
		h := newUploaderHarness(t, 1024, limits)
		originalFinish := h.api.finishLost
		_ = originalFinish
		// Mutate through a hook reached after the signed PUT but before verification.
		h.api.finishLost = false
		h.api.mu.Lock()
		h.api.committed = anyshare.Item{}
		h.api.mu.Unlock()
		mutating := &mutationAPI{UploadAPI: h.api, path: h.localPath}
		h.uploader.API = mutating
		_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
		assertUploadCategory(t, err, apperr.Local)
		if _, loadErr := h.states.Load(h.stateKey); loadErr != nil {
			t.Fatalf("state removed: %v", loadErr)
		}
	})
}

type cancelAfterAdvance struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (*cancelAfterAdvance) Started(int64)    {}
func (p *cancelAfterAdvance) Advanced(int64) { p.once.Do(p.cancel) }
func (*cancelAfterAdvance) Finished()        {}

type mutationAPI struct {
	UploadAPI
	path string
}

func (m *mutationAPI) FinishUpload(ctx context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	result, err := m.UploadAPI.FinishUpload(ctx, req)
	if err == nil {
		file, openErr := os.OpenFile(m.path, os.O_WRONLY, 0)
		if openErr != nil {
			return result, openErr
		}
		writeErr := file.Truncate(7)
		closeErr := file.Close()
		if writeErr != nil {
			return result, writeErr
		}
		if closeErr != nil {
			return result, closeErr
		}
	}
	return result, err
}

func TestUploaderRejectsInvalidResumeTuple(t *testing.T) {
	// Production mutation caught: trusting State.Matches while ignoring server/path/part-size/part-bound invariants.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	now := time.Now()
	bad := State{Server: "wrong-server", RemotePath: h.target.RemotePath, LocalPath: h.localPath, DocID: "doc-old", Rev: "rev-old", UploadID: "upload-old", Fingerprint: h.fingerprint, PartSize: 5 << 20, Phase: PhaseUploading,
		Completed: map[int]CompletedPart{99: {ETag: `"bad"`, Size: 1}}, CreatedAt: now, UpdatedAt: now}
	if err := h.states.Save(h.stateKey, bad); err != nil {
		t.Fatal(err)
	}
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resumed || h.api.initCalls != 1 {
		t.Fatalf("result=%+v init=%d", result, h.api.initCalls)
	}
}

func TestUploaderValidResumeSkipsInstantCreate(t *testing.T) {
	// Production mutation caught: checking PreUpload before reusable state and creating a duplicate file instead of resuming.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	now := time.Now()
	state := State{Server: h.uploader.Server, RemotePath: h.target.RemotePath, LocalPath: h.localPath, DocID: "doc-1", Rev: "rev-upload", UploadID: "upload-1", Phase: PhaseUploading,
		Fingerprint: h.fingerprint, PartSize: 5 << 20, Completed: map[int]CompletedPart{1: {ETag: `"etag-1"`, Size: 5 << 20}}, CreatedAt: now, UpdatedAt: now}
	if err := h.states.Save(h.stateKey, state); err != nil {
		t.Fatal(err)
	}
	h.api.match = true
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if h.api.directCalls != 0 || !result.Resumed || result.PartsResumed != 1 {
		t.Fatalf("result=%+v direct=%d", result, h.api.directCalls)
	}
}

func TestUploaderReportsResumedForValidatedEmptySession(t *testing.T) {
	// Production mutation caught: defining Resumed as "parts skipped" instead of reuse of a validated upload session.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	now := time.Now()
	state := State{Server: h.uploader.Server, RemotePath: h.target.RemotePath, LocalPath: h.localPath, DocID: "doc-1", Rev: "rev-upload", UploadID: "upload-1", Phase: PhaseUploading,
		Fingerprint: h.fingerprint, PartSize: 5 << 20, Completed: map[int]CompletedPart{}, CreatedAt: now, UpdatedAt: now}
	if err := h.states.Save(h.stateKey, state); err != nil {
		t.Fatal(err)
	}
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resumed || result.PartsResumed != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestUploaderReportsResumedSingleSession(t *testing.T) {
	// Production mutation caught: losing the Resumed flag when a validated single-object session is refreshed and completed.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	now := time.Now()
	state := State{Server: h.uploader.Server, RemotePath: h.target.RemotePath, LocalPath: h.localPath, DocID: "doc-1", Rev: "rev-upload", Phase: PhaseUploading,
		Fingerprint: h.fingerprint, Completed: map[int]CompletedPart{}, CreatedAt: now, UpdatedAt: now}
	if err := h.states.Save(h.stateKey, state); err != nil {
		t.Fatal(err)
	}
	h.api.match = true
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resumed || result.PartsTotal != 1 || result.PartsUploaded != 1 || h.api.directCalls != 0 || h.api.refreshes != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestUploaderNeverReplaysLegacyPhaseLessState(t *testing.T) {
	// Production mutation caught: treating an old phase-less state written after an ambiguous Finish as an ordinary resumable session and replaying Finish.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	now := time.Now()
	state := State{Server: h.uploader.Server, RemotePath: h.target.RemotePath, LocalPath: h.localPath, DocID: "doc-1", Rev: "rev-upload",
		Fingerprint: h.fingerprint, Completed: map[int]CompletedPart{}, CreatedAt: now, UpdatedAt: now}
	if err := h.states.Save(h.stateKey, state); err != nil {
		t.Fatal(err)
	}
	boundary := &legacyPhaseReplayAPI{UploadAPI: h.api}
	h.uploader.API = boundary
	h.uploader.Remote = &alwaysFailRemote{RemoteVerifier: h.remote}
	if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{}); err == nil {
		t.Fatal("expected inconclusive legacy-state reconciliation")
	}
	if boundary.refreshCalls != 0 || boundary.finishCalls != 0 {
		t.Fatalf("legacy state replayed: refresh=%d finish=%d", boundary.refreshCalls, boundary.finishCalls)
	}
	saved, err := h.states.Load(h.stateKey)
	if err != nil || saved.Phase != "" {
		t.Fatalf("legacy state=%+v err=%v", saved, err)
	}
}

type legacyPhaseReplayAPI struct {
	UploadAPI
	refreshCalls int
	finishCalls  int
}

func (a *legacyPhaseReplayAPI) RefreshUpload(ctx context.Context, docID, rev string, length int64, multipart bool) (anyshare.RefreshResult, error) {
	a.refreshCalls++
	return a.UploadAPI.RefreshUpload(ctx, docID, rev, length, multipart)
}

func (a *legacyPhaseReplayAPI) FinishUpload(ctx context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	a.finishCalls++
	return a.UploadAPI.FinishUpload(ctx, req)
}

func TestUploaderRequiresMatchingResolvedAndMetadataRevision(t *testing.T) {
	// Production mutation caught: accepting metadata for a different revision than the freshly resolved path.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.API = &metadataRevisionAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
	if _, loadErr := h.states.Load(h.stateKey); loadErr != nil {
		t.Fatalf("state removed: %v", loadErr)
	}
}

func TestUploaderRequiresResolvedPathAndName(t *testing.T) {
	// Production mutation caught: treating absent re-resolved path/name as if they proved the intended destination.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.Remote = &blankPathRemote{RemoteVerifier: h.remote}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
}

type blankPathRemote struct{ RemoteVerifier }

func (b *blankPathRemote) Resolve(ctx context.Context, path string) (anyshare.Item, error) {
	item, err := b.RemoteVerifier.Resolve(ctx, path)
	item.Path, item.Name = "", ""
	return item, err
}

type metadataRevisionAPI struct{ UploadAPI }

func (m *metadataRevisionAPI) FileMetadata(ctx context.Context, id, rev string) (anyshare.Item, error) {
	item, err := m.UploadAPI.FileMetadata(ctx, id, rev)
	item.Rev = "different-revision"
	return item, err
}

func TestUploaderChecksDescriptorWhenLostBeginIsReconciled(t *testing.T) {
	// Production mutation caught: returning reconciled begin success without checking the still-open local descriptor.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.beginLost = true
	h.uploader.API = &beginMutationAPI{UploadAPI: h.api, path: h.localPath}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Local)
}

func TestUploaderRejectsLostBeginEmptyResolvedIdentityBeforeMetadata(t *testing.T) {
	// Production mutation caught: reconciling a lost begin through metadata with an empty resolved ID and returning success with an empty Result.RemoteID.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	item := anyshare.Item{Rev: "rev-final", Name: h.target.Name, Path: h.target.RemotePath, Type: "file", Size: h.fingerprint.Size,
		ClientMtimeUS: h.fingerprint.MtimeNS / 1000, MD5: h.fingerprint.MD5, SliceMD5: h.fingerprint.SliceMD5, CRC32: h.fingerprint.CRC32}
	boundary := &emptyIdentityBeginAPI{UploadAPI: h.api, item: item}
	h.uploader.API = boundary
	h.uploader.Remote = &staticRemoteVerifier{item: item}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	if err == nil {
		t.Fatal("expected reconciliation failure")
	}
	if boundary.metadataCalls != 0 {
		t.Fatalf("metadata calls=%d", boundary.metadataCalls)
	}
}

func TestUploaderRejectsOverwriteBeginIdentityBeforeObjectWrite(t *testing.T) {
	// Production mutation caught: accepting an overwrite initialization for another document or the original revision and issuing signed object writes against it.
	tests := []struct {
		name      string
		multipart bool
		badID     string
		badRev    string
	}{
		{name: "single different document", badID: "other-doc"},
		{name: "single original revision", badRev: "original-rev"},
		{name: "multipart different document", multipart: true, badID: "other-doc"},
		{name: "multipart original revision", multipart: true, badRev: "original-rev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			size := int64(1024)
			if tt.multipart {
				size = 20 << 20
			}
			h := newUploaderHarness(t, size, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
			existing := anyshare.Item{ID: "old-doc", DocID: "old-doc", Rev: "original-rev", Name: h.target.Name, Path: h.target.RemotePath, Type: "file"}
			h.target.Existing = &existing
			h.api.target = h.target
			h.uploader.API = &badOverwriteBeginAPI{UploadAPI: h.api, badID: tt.badID, badRev: tt.badRev}
			_, err := h.uploader.Put(context.Background(), h.localPath, h.target, true, nopProgress{})
			assertUploadCategory(t, err, apperr.Integrity)
			if h.api.singlePuts != 0 || len(h.api.PutCalls()) != 0 || len(h.api.authorizeUploadIDs) != 0 {
				t.Fatalf("object write started: single=%d parts=%v authorizations=%v", h.api.singlePuts, h.api.PutCalls(), h.api.authorizeUploadIDs)
			}
			saved, loadErr := h.states.Load(h.stateKey)
			wantPhase := PhaseBeginSingleAttempted
			if tt.multipart {
				wantPhase = PhaseBeginMultipartAttempted
			}
			if loadErr != nil || saved.Phase != wantPhase || saved.DocID != "old-doc" || saved.EditedRev != "original-rev" {
				t.Fatalf("state=%+v err=%v", saved, loadErr)
			}
		})
	}
}

type badOverwriteBeginAPI struct {
	UploadAPI
	badID  string
	badRev string
}

func (a *badOverwriteBeginAPI) alter(begin anyshare.BeginUpload) anyshare.BeginUpload {
	if a.badID != "" {
		begin.DocID = a.badID
	}
	if a.badRev != "" {
		begin.Rev = a.badRev
	}
	return begin
}

func (a *badOverwriteBeginAPI) BeginSingle(ctx context.Context, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	begin, err := a.UploadAPI.BeginSingle(ctx, req)
	return a.alter(begin), err
}

func (a *badOverwriteBeginAPI) InitMultipart(ctx context.Context, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	begin, err := a.UploadAPI.InitMultipart(ctx, req)
	return a.alter(begin), err
}

type emptyIdentityBeginAPI struct {
	UploadAPI
	item          anyshare.Item
	metadataCalls int
}

func (e *emptyIdentityBeginAPI) BeginSingle(context.Context, anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	return anyshare.BeginUpload{}, networkFixture("lost begin response")
}
func (e *emptyIdentityBeginAPI) FileMetadata(context.Context, string, string) (anyshare.Item, error) {
	e.metadataCalls++
	return e.item, nil
}

type staticRemoteVerifier struct{ item anyshare.Item }

func (s *staticRemoteVerifier) Resolve(context.Context, string) (anyshare.Item, error) {
	return s.item, nil
}

func TestUploaderDoesNotReconcileLostOverwriteBeginToOriginalRevision(t *testing.T) {
	// Production mutation caught: mistaking the unchanged pre-upload overwrite target for a committed new revision after a lost begin response.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	existing := anyshare.Item{ID: "old-doc", DocID: "old-doc", Rev: "old-rev", Name: h.target.Name, Path: h.target.RemotePath, Type: "file", Size: h.fingerprint.Size,
		ClientMtimeUS: h.fingerprint.MtimeNS / 1000, MD5: h.fingerprint.MD5, SliceMD5: h.fingerprint.SliceMD5, CRC32: h.fingerprint.CRC32}
	h.target.Existing = &existing
	h.api.target = h.target
	h.api.committed = existing
	h.uploader.API = &lostBeginWithoutCommitAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, true, nopProgress{})
	assertUploadCategory(t, err, apperr.Network)
	if h.api.finishCalls != 0 {
		t.Fatalf("finish calls=%d", h.api.finishCalls)
	}
}

func TestUploaderDoesNotReconcileLostOverwriteFinishToOriginalRevision(t *testing.T) {
	// Production mutation caught: mistaking the unchanged original overwrite revision for a committed finish after a lost response.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	existing := anyshare.Item{ID: "old-doc", DocID: "old-doc", Rev: "old-rev", Name: h.target.Name, Path: h.target.RemotePath, Type: "file", Size: h.fingerprint.Size,
		ClientMtimeUS: h.fingerprint.MtimeNS / 1000, MD5: h.fingerprint.MD5, SliceMD5: h.fingerprint.SliceMD5, CRC32: h.fingerprint.CRC32}
	h.target.Existing = &existing
	h.api.target = h.target
	h.api.committed = existing
	h.uploader.API = &finishLostWithoutCommitAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, true, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
	if h.api.finishCalls != 0 {
		t.Fatalf("underlying finish calls=%d", h.api.finishCalls)
	}
}

type finishLostWithoutCommitAPI struct{ UploadAPI }

func (f *finishLostWithoutCommitAPI) FinishUpload(context.Context, anyshare.FinishRequest) (anyshare.UploadResult, error) {
	return anyshare.UploadResult{}, networkFixture("lost finish response")
}

func TestUploaderResumesOverwriteWithPersistedOriginalRevision(t *testing.T) {
	// Production mutation caught: restarting a valid overwrite session or finishing it against a revision other than its persisted begin-time baseline.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	existing := anyshare.Item{ID: "old-doc", Rev: "original-rev", Type: "file"}
	h.target.Existing = &existing
	h.api.target = h.target
	now := time.Now()
	state := State{Server: h.uploader.Server, RemotePath: h.target.RemotePath, LocalPath: h.localPath, DocID: "old-doc", Rev: "upload-rev", EditedRev: "original-rev", UploadID: "upload-1", Phase: PhaseUploading,
		Fingerprint: h.fingerprint, PartSize: 5 << 20, Completed: map[int]CompletedPart{1: {ETag: `"etag-1"`, Size: 5 << 20}}, CreatedAt: now, UpdatedAt: now}
	if err := h.states.Save(h.stateKey, state); err != nil {
		t.Fatal(err)
	}
	h.api.ResetPutCalls()
	result, err := h.uploader.Put(context.Background(), h.localPath, h.target, true, nopProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Resumed || result.PartsResumed != 1 || h.api.beginCalls != 0 || h.api.initCalls != 0 {
		t.Fatalf("result=%+v begins=%d inits=%d", result, h.api.beginCalls, h.api.initCalls)
	}
	if got := h.api.PutCalls(); !reflect.DeepEqual(got, []int{2, 3, 4}) {
		t.Fatalf("parts=%v", got)
	}
	if len(h.api.finishRequests) != 1 || h.api.finishRequests[0].EditedRev != "original-rev" {
		t.Fatalf("finish requests=%+v", h.api.finishRequests)
	}
}

func TestUploaderSerializesConcurrentProgress(t *testing.T) {
	// Production mutation caught: invoking a caller's ordinary Progress implementation concurrently from multipart workers.
	h := newUploaderHarness(t, 20<<20, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.Concurrency = 4
	h.uploader.API = newSimultaneousPartAPI(h.api, 4)
	progress := &overlapProgress{}
	if _, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, progress); err != nil {
		t.Fatal(err)
	}
	if progress.overlap.Load() {
		t.Fatal("Progress.Advanced calls overlapped")
	}
}

type simultaneousPartAPI struct {
	UploadAPI
	mu        sync.Mutex
	remaining int
	release   chan struct{}
}

func newSimultaneousPartAPI(api UploadAPI, count int) *simultaneousPartAPI {
	return &simultaneousPartAPI{UploadAPI: api, remaining: count, release: make(chan struct{})}
}
func (s *simultaneousPartAPI) PutSigned(ctx context.Context, req anyshare.SignedRequest, body io.Reader, reopen func() (io.ReadCloser, error), length int64) (string, error) {
	etag, err := s.UploadAPI.PutSigned(ctx, req, body, reopen, length)
	if _, parseErr := strconv.Atoi(filepath.Base(req.URL)); parseErr != nil {
		return etag, err
	}
	s.mu.Lock()
	s.remaining--
	if s.remaining == 0 {
		close(s.release)
	}
	release := s.release
	s.mu.Unlock()
	select {
	case <-release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return etag, err
}

type overlapProgress struct {
	active  atomic.Int32
	overlap atomic.Bool
}

func (*overlapProgress) Started(int64) {}
func (p *overlapProgress) Advanced(int64) {
	if p.active.Add(1) > 1 {
		p.overlap.Store(true)
	}
	for i := 0; i < 10000; i++ {
		runtime.Gosched()
	}
	p.active.Add(-1)
}
func (*overlapProgress) Finished() {}

type lostBeginWithoutCommitAPI struct{ UploadAPI }

func (l *lostBeginWithoutCommitAPI) BeginSingle(context.Context, anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	return anyshare.BeginUpload{}, networkFixture("lost begin response")
}

func TestUploaderClassifiesLostFinishMismatchAsIntegrity(t *testing.T) {
	// Production mutation caught: hiding a conclusively mismatched committed revision behind the original ambiguous network error.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.finishLost = true
	h.uploader.API = &corruptAfterFinishAPI{UploadAPI: h.api, api: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
	if _, loadErr := h.states.Load(h.stateKey); loadErr != nil {
		t.Fatalf("state removed: %v", loadErr)
	}
}

func TestUploaderRejectsFinishResultForDifferentID(t *testing.T) {
	// Production mutation caught: ignoring a FinishUpload result that identifies a different document than the initialized upload.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.API = &wrongFinishIDAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
}

func TestUploaderRejectsOverwriteFinishAtOriginalRevision(t *testing.T) {
	// Production mutation caught: accepting a no-op overwrite finish that returns and verifies the unchanged original revision.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	existing := anyshare.Item{ID: "old-doc", DocID: "old-doc", Rev: "original-rev", Name: h.target.Name, Path: h.target.RemotePath, Type: "file", Size: h.fingerprint.Size,
		ClientMtimeUS: h.fingerprint.MtimeNS / 1000, MD5: h.fingerprint.MD5, SliceMD5: h.fingerprint.SliceMD5, CRC32: h.fingerprint.CRC32}
	h.target.Existing = &existing
	h.api.target = h.target
	h.uploader.API = &noopOverwriteFinishAPI{UploadAPI: h.api, api: h.api, original: existing}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, true, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
	if _, loadErr := h.states.Load(h.stateKey); loadErr != nil {
		t.Fatalf("state removed: %v", loadErr)
	}
}

func TestUploaderRequiresFinishRevisionToEqualUploadState(t *testing.T) {
	// Production mutation caught: adopting a concurrent matching revision instead of the exact revision initialized in upload state.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.API = &differentFinishRevisionAPI{UploadAPI: h.api, api: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
	saved, loadErr := h.states.Load(h.stateKey)
	if loadErr != nil || saved.Phase != PhaseFinishAttempted {
		t.Fatalf("state=%+v err=%v", saved, loadErr)
	}
}

func TestUploaderRequiresAmbiguousFinishReconciliationAtUploadRevision(t *testing.T) {
	// Production mutation caught: reconciling an ambiguous finish to an unrelated concurrent revision with identical content.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.uploader.API = &ambiguousConcurrentFinishAPI{UploadAPI: h.api, api: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
	saved, loadErr := h.states.Load(h.stateKey)
	if loadErr != nil || saved.Phase != PhaseFinishAttempted {
		t.Fatalf("state=%+v err=%v", saved, loadErr)
	}
}

type ambiguousConcurrentFinishAPI struct {
	UploadAPI
	api *fakeUploadAPI
}

func (a *ambiguousConcurrentFinishAPI) FinishUpload(_ context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	a.api.mu.Lock()
	a.api.finishCalls++
	a.api.commit(req.DocID, "concurrent-rev")
	a.api.mu.Unlock()
	return anyshare.UploadResult{}, networkFixture("lost finish response")
}

type differentFinishRevisionAPI struct {
	UploadAPI
	api *fakeUploadAPI
}

func (d *differentFinishRevisionAPI) FinishUpload(_ context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	d.api.mu.Lock()
	d.api.finishCalls++
	d.api.commit(req.DocID, "concurrent-rev")
	d.api.mu.Unlock()
	return anyshare.UploadResult{DocID: req.DocID, Rev: "concurrent-rev", Name: d.api.target.Name}, nil
}

type noopOverwriteFinishAPI struct {
	UploadAPI
	api      *fakeUploadAPI
	original anyshare.Item
}

func (n *noopOverwriteFinishAPI) FinishUpload(context.Context, anyshare.FinishRequest) (anyshare.UploadResult, error) {
	n.api.mu.Lock()
	n.api.finishCalls++
	n.api.committed = n.original
	n.api.mu.Unlock()
	return anyshare.UploadResult{DocID: n.original.ID, Rev: n.original.Rev, Name: n.original.Name}, nil
}

type wrongFinishIDAPI struct{ UploadAPI }

func (w *wrongFinishIDAPI) FinishUpload(ctx context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	result, err := w.UploadAPI.FinishUpload(ctx, req)
	result.DocID = "different-doc"
	return result, err
}

func TestUploaderRejectsEmptyDirectResultIdentity(t *testing.T) {
	// Production mutation caught: accepting a successful DirectUpload response with no returned document identity or revision.
	h := newUploaderHarness(t, 1024, Limits{PartMinSize: 5 << 20, PartMaxSize: 5 << 20, PartMaxNum: 100})
	h.api.match = true
	h.uploader.API = &emptyDirectResultAPI{UploadAPI: h.api}
	_, err := h.uploader.Put(context.Background(), h.localPath, h.target, false, nopProgress{})
	assertUploadCategory(t, err, apperr.Integrity)
}

type emptyDirectResultAPI struct{ UploadAPI }

func (e *emptyDirectResultAPI) DirectUpload(ctx context.Context, req anyshare.DirectUploadRequest) (anyshare.UploadResult, error) {
	_, err := e.UploadAPI.DirectUpload(ctx, req)
	return anyshare.UploadResult{}, err
}

type corruptAfterFinishAPI struct {
	UploadAPI
	api *fakeUploadAPI
}

func (c *corruptAfterFinishAPI) FinishUpload(ctx context.Context, req anyshare.FinishRequest) (anyshare.UploadResult, error) {
	result, err := c.UploadAPI.FinishUpload(ctx, req)
	c.api.mu.Lock()
	c.api.committed.MD5 = "00000000000000000000000000000000"
	c.api.mu.Unlock()
	return result, err
}

type beginMutationAPI struct {
	UploadAPI
	path string
}

func (m *beginMutationAPI) BeginSingle(ctx context.Context, req anyshare.BeginRequest) (anyshare.BeginUpload, error) {
	result, err := m.UploadAPI.BeginSingle(ctx, req)
	file, openErr := os.OpenFile(m.path, os.O_WRONLY, 0)
	if openErr != nil {
		return result, openErr
	}
	writeErr := file.Truncate(7)
	closeErr := file.Close()
	if writeErr != nil {
		return result, writeErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	return result, err
}

func networkFixture(message string) error {
	return apperr.Wrap(apperr.Network, "fixture", message, errors.New("synthetic failure"))
}

func assertUploadCategory(t *testing.T, err error, want apperr.Category) {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Category != want {
		t.Fatalf("err=%v category=%v want=%v", err, appErr, want)
	}
}
