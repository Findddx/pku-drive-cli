package upload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Findddx/pku-drive-cli/internal/anyshare"
	"github.com/Findddx/pku-drive-cli/internal/apperr"
	"github.com/Findddx/pku-drive-cli/internal/remote"
)

// Progress receives aggregate byte progress for one upload.
type Progress interface {
	Started(total int64)
	Advanced(delta int64)
	Finished()
}

// UploadAPI is the AnyShare and signed-object API needed by Uploader.
type UploadAPI interface {
	StorageOptions(context.Context) (anyshare.StorageOptions, error)
	PreUpload(context.Context, int64, string) (bool, error)
	DirectUpload(context.Context, anyshare.DirectUploadRequest) (anyshare.UploadResult, error)
	BeginSingle(context.Context, anyshare.BeginRequest) (anyshare.BeginUpload, error)
	InitMultipart(context.Context, anyshare.BeginRequest) (anyshare.BeginUpload, error)
	AuthorizeParts(context.Context, string, string, string, int, int) (anyshare.PartAuthorization, error)
	CompleteMultipart(context.Context, string, string, string, map[int]anyshare.PartInfo) (anyshare.SignedRequest, []byte, error)
	RefreshUpload(context.Context, string, string, int64, bool) (anyshare.RefreshResult, error)
	FinishUpload(context.Context, anyshare.FinishRequest) (anyshare.UploadResult, error)
	FileMetadata(context.Context, string, string) (anyshare.Item, error)
	PutSigned(context.Context, anyshare.SignedRequest, io.Reader, func() (io.ReadCloser, error), int64) (string, error)
}

// RemoteVerifier resolves a visible remote path without retaining cached IDs.
type RemoteVerifier interface {
	Resolve(ctx context.Context, remotePath string) (anyshare.Item, error)
}

// Result describes a successfully verified upload.
type Result struct {
	RemotePath    string
	RemoteID      string
	Revision      string
	Size          int64
	Resumed       bool
	Instant       bool
	Multipart     bool
	PartsTotal    int
	PartsResumed  int
	PartsUploaded int
}

// Uploader coordinates fingerprinting, resumable object upload, and verification.
type Uploader struct {
	Server      string
	API         UploadAPI
	Remote      RemoteVerifier
	States      *StateStore
	Concurrency int
	Now         func() time.Time

	sleep func(context.Context, time.Duration) error
}

// NewUploader constructs an upload coordinator.
func NewUploader(server string, api UploadAPI, remoteVerifier RemoteVerifier, states *StateStore, concurrency int) *Uploader {
	return &Uploader{
		Server: server, API: api, Remote: remoteVerifier, States: states, Concurrency: concurrency,
		Now: time.Now, sleep: contextSleep,
	}
}

// Put uploads one local regular file to a previously resolved target.
func (u *Uploader) Put(ctx context.Context, localPath string, target remote.UploadTarget, overwrite bool, progress Progress) (Result, error) {
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return Result{}, uploadError(apperr.Local, "resolve local upload path", err)
	}
	normalized, err := remote.Normalize(target.RemotePath)
	if err != nil {
		return Result{}, err
	}
	key := StateKey(u.Server, normalized, abs)
	if u == nil || u.API == nil || u.Remote == nil || u.States == nil {
		return Result{}, uploadError(apperr.Local, "configure upload", errors.New("missing upload dependency"))
	}
	var result Result
	err = u.States.WithUploadLock(ctx, key, func() error {
		var inner error
		result, inner = u.putLocked(ctx, abs, key, normalized, target, overwrite, progress)
		return inner
	})
	if err != nil {
		return Result{}, classifyUploadError("upload file", err)
	}
	return result, nil
}

func (u *Uploader) putLocked(ctx context.Context, abs, key, normalized string, target remote.UploadTarget, overwrite bool, progress Progress) (Result, error) {
	file, fingerprint, err := OpenFingerprint(abs)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()

	pendingState, pendingErr := u.States.Load(key)
	if pendingErr == nil && pendingState.Phase != PhaseUploading {
		legacyPhaseLess := pendingState.Phase == ""
		if !legacyPhaseLess && !isAttemptedPhase(pendingState.Phase) {
			return Result{}, integrityError("upload state has unknown operation phase")
		}
		if progress != nil {
			progress.Started(fingerprint.Size)
		}
		options, err := u.API.StorageOptions(ctx)
		if err != nil {
			return Result{}, classifyUploadError("obtain upload limits", err)
		}
		plan, err := BuildPlan(fingerprint.Size, Limits{PartMinSize: options.PartMinSize, PartMaxSize: options.PartMaxSize, PartMaxNum: options.PartMaxNum}, u.Concurrency)
		if err != nil {
			return Result{}, err
		}
		validShape := attemptedStateValid(pendingState, plan)
		if legacyPhaseLess {
			validShape = ordinaryStateShapeValid(pendingState, plan)
		}
		if !pendingStateBaseValid(pendingState, u.Server, normalized, abs, fingerprint, plan) || !validShape {
			return Result{}, integrityError("attempted upload state does not match current invocation")
		}
		if legacyPhaseLess {
			// Old states did not record whether FinishUpload had already been
			// attempted. Treat them as finish-attempted in memory so an upgrade
			// can only reconcile the exact stored identity, never replay it.
			pendingState.Phase = PhaseFinishAttempted
		}
		return u.reconcilePending(ctx, file, fingerprint, key, normalized, filepath.Base(normalized), pendingState, plan, progress)
	}
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return Result{}, pendingErr
	}

	if target.Existing != nil && !overwrite {
		return Result{}, uploadError(apperr.Remote, "upload target exists", errors.New("overwrite is required"))
	}
	if progress != nil {
		progress.Started(fingerprint.Size)
	}
	options, err := u.API.StorageOptions(ctx)
	if err != nil {
		return Result{}, classifyUploadError("obtain upload limits", err)
	}
	plan, err := BuildPlan(fingerprint.Size, Limits{PartMinSize: options.PartMinSize, PartMaxSize: options.PartMaxSize, PartMaxNum: options.PartMaxNum}, u.Concurrency)
	if err != nil {
		return Result{}, err
	}
	checksums := fingerprintChecksums(fingerprint)
	mtimeUS := fingerprint.MtimeNS / 1000
	state, resumed, err := u.loadValidState(key, normalized, abs, fingerprint, plan, target)
	if err != nil {
		return Result{}, err
	}

	if !resumed && target.Existing == nil {
		match, preErr := u.API.PreUpload(ctx, fingerprint.Size, fingerprint.SliceMD5)
		if preErr != nil {
			return Result{}, classifyUploadError("check instant upload", preErr)
		}
		if match {
			state = u.newAttemptState(normalized, abs, fingerprint, plan, target, PhaseDirectAttempted)
			if err := u.States.Save(key, state); err != nil {
				return Result{}, err
			}
			direct, directErr := u.API.DirectUpload(ctx, anyshare.DirectUploadRequest{
				ParentID: target.ParentID, Name: target.Name, Size: fingerprint.Size,
				ClientMtimeUS: mtimeUS, Checksums: checksums,
			})
			if directErr != nil {
				if !ambiguousUploadError(directErr) {
					if removeErr := u.States.Remove(key); removeErr != nil {
						return Result{}, removeErr
					}
					return Result{}, classifyUploadError("instant upload", directErr)
				}
				item, reconcileErr := u.reconcile(ctx, normalized, "", "", "", fingerprint)
				if reconcileErr != nil {
					if errorCategory(reconcileErr) == apperr.Integrity {
						return Result{}, reconcileErr
					}
					if interruptedUploadError(reconcileErr) {
						return Result{}, classifyUploadError("reconcile instant upload", reconcileErr)
					}
					return Result{}, classifyUploadError("instant upload", directErr)
				}
				direct = anyshare.UploadResult{DocID: itemIdentity(item), Rev: item.Rev, Name: item.Name}
			}
			if direct.DocID == "" || direct.Rev == "" {
				return Result{}, integrityError("instant upload result is missing identity")
			}
			verified, verifyErr := u.verify(ctx, file, fingerprint, normalized, target.Name, direct.DocID, direct.Rev)
			if verifyErr != nil {
				return Result{}, verifyErr
			}
			if err := u.States.Remove(key); err != nil {
				return Result{}, err
			}
			if progress != nil {
				progress.Finished()
			}
			return resultFromItem(verified, normalized, fingerprint.Size, true, false, 0, 0, 0, false), nil
		}
	}

	beginReq := anyshare.BeginRequest{ParentID: target.ParentID, Name: target.Name, Size: fingerprint.Size, ClientMtimeUS: mtimeUS}
	if target.Existing != nil {
		beginReq.ParentID, beginReq.Name = "", ""
		beginReq.ExistingID, beginReq.EditedRev = target.Existing.ID, target.Existing.Rev
	}

	if !resumed {
		phase := PhaseBeginSingleAttempted
		if plan.Multipart {
			phase = PhaseBeginMultipartAttempted
		}
		state = u.newAttemptState(normalized, abs, fingerprint, plan, target, phase)
		if err := u.States.Save(key, state); err != nil {
			return Result{}, err
		}
		var begin anyshare.BeginUpload
		if plan.Multipart {
			begin, err = u.API.InitMultipart(ctx, beginReq)
		} else {
			begin, err = u.API.BeginSingle(ctx, beginReq)
		}
		if err != nil {
			if ambiguousUploadError(err) {
				expectedID := ""
				disallowedRev := ""
				if target.Existing != nil {
					expectedID = target.Existing.ID
					disallowedRev = target.Existing.Rev
				}
				if item, reconcileErr := u.reconcile(ctx, normalized, expectedID, "", disallowedRev, fingerprint); reconcileErr == nil {
					verified, verifyErr := u.verify(ctx, file, fingerprint, normalized, target.Name, itemIdentity(item), item.Rev)
					if verifyErr != nil {
						return Result{}, verifyErr
					}
					if removeErr := u.States.Remove(key); removeErr != nil {
						return Result{}, removeErr
					}
					if progress != nil {
						progress.Finished()
					}
					return resultFromItem(verified, normalized, fingerprint.Size, false, plan.Multipart, plan.PartCount, 0, 0, false), nil
				} else {
					if errorCategory(reconcileErr) == apperr.Integrity {
						return Result{}, reconcileErr
					}
					if interruptedUploadError(reconcileErr) {
						return Result{}, classifyUploadError("reconcile upload initialization", reconcileErr)
					}
				}
			}
			if !ambiguousUploadError(err) {
				if removeErr := u.States.Remove(key); removeErr != nil {
					return Result{}, removeErr
				}
			}
			return Result{}, classifyUploadError("initialize upload", err)
		}
		if begin.DocID == "" || begin.Rev == "" {
			return Result{}, integrityError("upload initialization result is missing identity")
		}
		if target.Existing != nil && (begin.DocID != state.DocID || begin.Rev == state.EditedRev) {
			return Result{}, integrityError("overwrite initialization identity mismatch")
		}
		state.DocID, state.Rev, state.UploadID = begin.DocID, begin.Rev, begin.UploadID
		state.Phase = PhaseUploading
		state.UpdatedAt = u.now()
		if err := u.States.Save(key, state); err != nil {
			return Result{}, err
		}
		if !plan.Multipart {
			if begin.Request == nil {
				return Result{}, uploadError(apperr.Remote, "initialize upload", errors.New("missing signed request"))
			}
			return u.putSingle(ctx, file, fingerprint, key, normalized, target.Name, state.EditedRev, state, *begin.Request, false, progress)
		}
	}
	if plan.Multipart {
		return u.putMultipart(ctx, file, fingerprint, key, normalized, target.Name, state.EditedRev, state, plan, resumed, progress)
	}
	refresh, err := u.API.RefreshUpload(ctx, state.DocID, state.Rev, fingerprint.Size, false)
	if err != nil || refresh.Request == nil {
		if err == nil {
			err = errors.New("missing refreshed signed request")
		}
		return Result{}, classifyUploadError("refresh upload", err)
	}
	return u.putSingle(ctx, file, fingerprint, key, normalized, target.Name, state.EditedRev, state, *refresh.Request, true, progress)
}

func (u *Uploader) putSingle(ctx context.Context, file *os.File, fingerprint Fingerprint, key, normalized, name, editedRev string, state State, signed anyshare.SignedRequest, resumed bool, progress Progress) (Result, error) {
	etag, err := u.putSection(ctx, file, signed, 0, fingerprint.Size)
	_ = etag
	if anyshare.IsExpiredSignature(err) {
		refresh, refreshErr := u.API.RefreshUpload(ctx, state.DocID, state.Rev, fingerprint.Size, false)
		if refreshErr != nil {
			return Result{}, classifyUploadError("refresh upload", refreshErr)
		}
		if refresh.Request == nil {
			return Result{}, uploadError(apperr.Remote, "refresh upload", errors.New("missing signed request"))
		}
		_, err = u.putSection(ctx, file, *refresh.Request, 0, fingerprint.Size)
	}
	if err != nil {
		return Result{}, classifyUploadError("upload object", err)
	}
	if progress != nil {
		progress.Advanced(fingerprint.Size)
	}
	item, err := u.finishAndVerify(ctx, file, fingerprint, key, normalized, name, editedRev, state)
	if err != nil {
		return Result{}, err
	}
	if progress != nil {
		progress.Finished()
	}
	return resultFromItem(item, normalized, fingerprint.Size, false, false, 1, 0, 1, resumed), nil
}

func (u *Uploader) putMultipart(ctx context.Context, file *os.File, fingerprint Fingerprint, key, normalized, name, editedRev string, state State, plan Plan, resumed bool, progress Progress) (Result, error) {
	initialCompleted := len(state.Completed)
	uploaded := make(map[int]bool)
	partRefreshed := false
	completionRefreshed := false
	for {
		expired, err := u.uploadMissingParts(ctx, file, key, &state, plan, uploaded, progress)
		if err != nil {
			return Result{}, err
		}
		if expired {
			if partRefreshed {
				return Result{}, uploadError(apperr.Remote, "upload multipart part", anyshare.ErrExpiredSignature)
			}
			partRefreshed = true
			changed, refreshErr := u.refreshMultipart(ctx, key, &state, fingerprint.Size)
			if refreshErr != nil {
				return Result{}, refreshErr
			}
			if changed {
				initialCompleted = 0
				uploaded = make(map[int]bool)
			}
			continue
		}

		parts := make(map[int]anyshare.PartInfo, len(state.Completed))
		for part, completed := range state.Completed {
			parts[part] = anyshare.PartInfo{ETag: completed.ETag, Size: completed.Size}
		}
		signed, body, err := u.API.CompleteMultipart(ctx, state.DocID, state.Rev, state.UploadID, parts)
		if err != nil {
			return Result{}, classifyUploadError("complete multipart upload", err)
		}
		_, err = u.API.PutSigned(ctx, signed, bytes.NewReader(body), func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}, int64(len(body)))
		if anyshare.IsExpiredSignature(err) {
			if completionRefreshed {
				return Result{}, uploadError(apperr.Remote, "complete multipart upload", err)
			}
			completionRefreshed = true
			changed, refreshErr := u.refreshMultipart(ctx, key, &state, fingerprint.Size)
			if refreshErr != nil {
				return Result{}, refreshErr
			}
			if changed {
				initialCompleted = 0
				uploaded = make(map[int]bool)
			}
			continue
		}
		if err != nil {
			return Result{}, classifyUploadError("complete multipart object", err)
		}
		break
	}
	item, err := u.finishAndVerify(ctx, file, fingerprint, key, normalized, name, editedRev, state)
	if err != nil {
		return Result{}, err
	}
	if progress != nil {
		progress.Finished()
	}
	return resultFromItem(item, normalized, fingerprint.Size, false, true, plan.PartCount, initialCompleted, len(uploaded), resumed), nil
}

func (u *Uploader) uploadMissingParts(ctx context.Context, file *os.File, key string, state *State, plan Plan, uploaded map[int]bool, progress Progress) (bool, error) {
	type job struct {
		part   int
		signed anyshare.SignedRequest
	}
	missing := make([]int, 0, plan.PartCount-len(state.Completed))
	for part := 1; part <= plan.PartCount; part++ {
		if _, ok := state.Completed[part]; ok {
			continue
		}
		missing = append(missing, part)
	}
	if len(missing) == 0 {
		return false, nil
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobCh := make(chan job)
	type outcome struct {
		expired bool
		err     error
	}
	// At most one outcome per worker plus a simultaneous producer failure can
	// be reported before the coordinator starts draining this channel.
	outcomes := make(chan outcome, plan.Concurrency+1)
	var stateMu sync.Mutex
	var progressMu sync.Mutex
	var workers sync.WaitGroup
	worker := func() {
		defer workers.Done()
		for job := range jobCh {
			if workerCtx.Err() != nil {
				return
			}
			offset := int64(job.part-1) * plan.PartSize
			length := partLength(state.Fingerprint.Size, plan.PartSize, job.part)
			etag, err := u.putSection(workerCtx, file, job.signed, offset, length)
			if err != nil {
				if anyshare.IsExpiredSignature(err) {
					outcomes <- outcome{expired: true}
					cancel()
					return
				}
				if workerCtx.Err() != nil && ctx.Err() == nil {
					return
				}
				outcomes <- outcome{err: classifyUploadError("upload multipart part", err)}
				cancel()
				return
			}
			stateMu.Lock()
			state.Completed[job.part] = CompletedPart{ETag: etag, Size: length}
			state.UpdatedAt = u.now()
			err = u.States.Save(key, *state)
			if err == nil {
				uploaded[job.part] = true
			}
			stateMu.Unlock()
			if err != nil {
				outcomes <- outcome{err: err}
				cancel()
				return
			}
			if progress != nil {
				progressMu.Lock()
				progress.Advanced(length)
				progressMu.Unlock()
			}
		}
	}
	for i := 0; i < plan.Concurrency; i++ {
		workers.Add(1)
		go worker()
	}

	// Authorize each part only when a worker is ready to accept it. Because the
	// job channel is unbuffered, no more than the active workers plus the one
	// request currently waiting to be handed off can hold unused signed URLs.
produce:
	for _, part := range missing {
		if workerCtx.Err() != nil {
			break
		}
		auth, err := u.API.AuthorizeParts(workerCtx, state.DocID, state.Rev, state.UploadID, part, part)
		if err != nil {
			if workerCtx.Err() == nil {
				outcomes <- outcome{err: classifyUploadError("authorize upload part", err)}
				cancel()
			}
			break
		}
		signed, ok := auth[part]
		if !ok {
			outcomes <- outcome{err: uploadError(apperr.Remote, "authorize upload part", errors.New("missing part authorization"))}
			cancel()
			break
		}
		select {
		case jobCh <- job{part: part, signed: signed}:
		case <-workerCtx.Done():
			break produce
		}
	}
	close(jobCh)
	workers.Wait()
	cancel()
	close(outcomes)
	var firstErr error
	expired := false
	for outcome := range outcomes {
		if outcome.expired {
			expired = true
		}
		if firstErr == nil && outcome.err != nil {
			firstErr = outcome.err
		}
	}
	if ctx.Err() != nil {
		return false, uploadError(apperr.Interrupted, "upload multipart", ctx.Err())
	}
	if firstErr != nil {
		return false, firstErr
	}
	return expired, nil
}

func (u *Uploader) refreshMultipart(ctx context.Context, key string, state *State, size int64) (bool, error) {
	refresh, err := u.API.RefreshUpload(ctx, state.DocID, state.Rev, size, true)
	if err != nil {
		return false, classifyUploadError("refresh multipart upload", err)
	}
	if refresh.UploadID == "" {
		return false, uploadError(apperr.Remote, "refresh multipart upload", errors.New("missing upload ID"))
	}
	changed := refresh.UploadID != state.UploadID
	state.UploadID = refresh.UploadID
	if changed {
		state.Completed = make(map[int]CompletedPart)
	}
	state.UpdatedAt = u.now()
	if err := u.States.Save(key, *state); err != nil {
		return false, err
	}
	return changed, nil
}

func (u *Uploader) finishAndVerify(ctx context.Context, file *os.File, fingerprint Fingerprint, key, normalized, name, editedRev string, state State) (anyshare.Item, error) {
	state.Phase = PhaseFinishAttempted
	state.UpdatedAt = u.now()
	if err := u.States.Save(key, state); err != nil {
		return anyshare.Item{}, err
	}
	finished, err := u.API.FinishUpload(ctx, anyshare.FinishRequest{DocID: state.DocID, Rev: state.Rev, EditedRev: editedRev, Checksums: fingerprintChecksums(fingerprint)})
	if err != nil {
		if !ambiguousUploadError(err) {
			state.Phase = PhaseUploading
			state.UpdatedAt = u.now()
			if saveErr := u.States.Save(key, state); saveErr != nil {
				return anyshare.Item{}, saveErr
			}
			return anyshare.Item{}, classifyUploadError("finish upload", err)
		}
		item, reconcileErr := u.reconcile(ctx, normalized, state.DocID, state.Rev, editedRev, fingerprint)
		if reconcileErr != nil {
			if errorCategory(reconcileErr) == apperr.Integrity {
				return anyshare.Item{}, reconcileErr
			}
			if interruptedUploadError(reconcileErr) {
				return anyshare.Item{}, classifyUploadError("reconcile finished upload", reconcileErr)
			}
			return anyshare.Item{}, classifyUploadError("finish upload", err)
		}
		unchanged, localErr := fingerprint.Unchanged(file)
		if localErr != nil {
			return anyshare.Item{}, localErr
		}
		if !unchanged {
			return anyshare.Item{}, uploadError(apperr.Local, "verify local upload file", errors.New("file changed during upload"))
		}
		if err := u.States.Remove(key); err != nil {
			return anyshare.Item{}, err
		}
		return item, nil
	}
	if finished.DocID == "" || finished.DocID != state.DocID || finished.Rev == "" || finished.Rev != state.Rev {
		return anyshare.Item{}, integrityError("finish result identity mismatch")
	}
	if editedRev != "" && finished.Rev == editedRev {
		return anyshare.Item{}, integrityError("overwrite did not create a new revision")
	}
	item, err := u.verify(ctx, file, fingerprint, normalized, name, state.DocID, finished.Rev)
	if err != nil {
		return anyshare.Item{}, err
	}
	if err := u.States.Remove(key); err != nil {
		return anyshare.Item{}, err
	}
	return item, nil
}

func (u *Uploader) verify(ctx context.Context, file *os.File, fingerprint Fingerprint, normalized, name, expectedID, expectedRev string) (anyshare.Item, error) {
	unchanged, err := fingerprint.Unchanged(file)
	if err != nil {
		return anyshare.Item{}, err
	}
	if !unchanged {
		return anyshare.Item{}, uploadError(apperr.Local, "verify local upload file", errors.New("file changed during upload"))
	}
	resolved, err := u.Remote.Resolve(ctx, normalized)
	if err != nil {
		return anyshare.Item{}, classifyUploadError("verify remote upload", err)
	}
	resolvedID := itemIdentity(resolved)
	if expectedID != "" && resolvedID != expectedID {
		return anyshare.Item{}, integrityError("remote ID mismatch")
	}
	if expectedRev != "" && resolved.Rev != expectedRev {
		return anyshare.Item{}, integrityError("remote revision mismatch")
	}
	if err := validateResolvedItem(resolved, normalized, name, fingerprint, resolvedID); err != nil {
		return anyshare.Item{}, err
	}
	metadata, err := u.API.FileMetadata(ctx, resolvedID, resolved.Rev)
	if err != nil {
		return anyshare.Item{}, classifyUploadError("verify remote metadata", err)
	}
	if err := validateRemoteItem(metadata, normalized, name, fingerprint, resolvedID); err != nil {
		return anyshare.Item{}, err
	}
	if resolved.Rev == "" || metadata.Rev != resolved.Rev {
		return anyshare.Item{}, integrityError("remote revision mismatch")
	}
	if metadata.Path == "" {
		metadata.Path = normalized
	}
	if metadata.Name == "" {
		metadata.Name = name
	}
	return metadata, nil
}

func (u *Uploader) reconcile(ctx context.Context, normalized, expectedID, expectedRev, disallowedRev string, fingerprint Fingerprint) (anyshare.Item, error) {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			delay := 200 * time.Millisecond << (attempt - 1)
			if err := u.wait(ctx, delay); err != nil {
				return anyshare.Item{}, err
			}
		}
		resolved, err := u.Remote.Resolve(ctx, normalized)
		if err != nil {
			last = err
			continue
		}
		id := itemIdentity(resolved)
		if id == "" || resolved.Rev == "" {
			last = integrityError("remote identity is incomplete")
			continue
		}
		if expectedID != "" && id != expectedID {
			last = integrityError("remote ID mismatch")
			continue
		}
		if expectedRev != "" && resolved.Rev != expectedRev {
			last = integrityError("remote revision mismatch")
			continue
		}
		if disallowedRev != "" && resolved.Rev == disallowedRev {
			last = errors.New("remote revision is unchanged")
			continue
		}
		metadata, err := u.API.FileMetadata(ctx, id, resolved.Rev)
		if err != nil {
			last = err
			continue
		}
		if err := validateResolvedItem(resolved, normalized, filepath.Base(normalized), fingerprint, id); err != nil {
			last = err
			continue
		}
		if err := validateRemoteItem(metadata, normalized, filepath.Base(normalized), fingerprint, id); err != nil {
			last = err
			continue
		}
		if resolved.Rev == "" || metadata.Rev != resolved.Rev {
			last = integrityError("remote revision mismatch")
			continue
		}
		return metadata, nil
	}
	if last == nil {
		last = errors.New("remote upload not found")
	}
	return anyshare.Item{}, last
}

func (u *Uploader) reconcilePending(ctx context.Context, file *os.File, fingerprint Fingerprint, key, normalized, name string, state State, plan Plan, progress Progress) (Result, error) {
	expectedID, expectedRev, disallowedRev := "", "", state.EditedRev
	instant := state.Phase == PhaseDirectAttempted
	if state.Phase == PhaseBeginSingleAttempted || state.Phase == PhaseBeginMultipartAttempted {
		expectedID = state.DocID
	}
	if state.Phase == PhaseFinishAttempted {
		expectedID, expectedRev = state.DocID, state.Rev
	}
	item, err := u.reconcile(ctx, normalized, expectedID, expectedRev, disallowedRev, state.Fingerprint)
	if err != nil {
		if errorCategory(err) == apperr.Integrity {
			return Result{}, err
		}
		return Result{}, classifyUploadError("reconcile pending upload", err)
	}
	verified, err := u.verify(ctx, file, fingerprint, normalized, name, itemIdentity(item), item.Rev)
	if err != nil {
		return Result{}, err
	}
	if err := u.States.Remove(key); err != nil {
		return Result{}, err
	}
	if progress != nil {
		progress.Finished()
	}
	partsTotal, partsResumed := 0, 0
	multipart := plan.Multipart && !instant
	if !instant {
		if multipart {
			partsTotal = plan.PartCount
		} else {
			partsTotal = 1
		}
		if state.Phase == PhaseFinishAttempted && multipart {
			partsResumed = len(state.Completed)
		}
	}
	return resultFromItem(verified, normalized, fingerprint.Size, instant, multipart, partsTotal, partsResumed, 0, true), nil
}

func (u *Uploader) loadValidState(key, normalized, abs string, fingerprint Fingerprint, plan Plan, target remote.UploadTarget) (State, bool, error) {
	state, err := u.States.Load(key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, false, nil
		}
		return State{}, false, err
	}
	baseValid := state.Server == u.Server && state.RemotePath == normalized && state.LocalPath == abs && state.Matches(fingerprint) &&
		state.PartSize == plan.PartSize && state.Completed != nil
	if isAttemptedPhase(state.Phase) {
		if !baseValid || !attemptedStateValid(state, plan) {
			return State{}, false, integrityError("attempted upload state does not match current invocation")
		}
		return state, true, nil
	}
	if state.Phase != PhaseUploading {
		return State{}, false, integrityError("upload state has unknown operation phase")
	}
	valid := baseValid && ordinaryStateValid(state, plan, target)
	if valid {
		return state, true, nil
	}
	if err := u.States.Remove(key); err != nil {
		return State{}, false, err
	}
	return State{}, false, nil
}

func (u *Uploader) newAttemptState(normalized, abs string, fingerprint Fingerprint, plan Plan, target remote.UploadTarget, phase OperationPhase) State {
	now := u.now()
	state := State{Server: u.Server, RemotePath: normalized, LocalPath: abs, Fingerprint: fingerprint, PartSize: plan.PartSize,
		Completed: make(map[int]CompletedPart), Phase: phase, CreatedAt: now, UpdatedAt: now}
	if target.Existing != nil {
		state.DocID = target.Existing.ID
		state.EditedRev = target.Existing.Rev
	}
	return state
}

func isAttemptedPhase(phase OperationPhase) bool {
	switch phase {
	case PhaseDirectAttempted, PhaseBeginSingleAttempted, PhaseBeginMultipartAttempted, PhaseFinishAttempted:
		return true
	default:
		return false
	}
}

func attemptedStateValid(state State, plan Plan) bool {
	switch state.Phase {
	case PhaseDirectAttempted:
		return state.DocID == "" && state.Rev == "" && state.EditedRev == "" && state.UploadID == "" && len(state.Completed) == 0
	case PhaseBeginSingleAttempted, PhaseBeginMultipartAttempted:
		if (state.Phase == PhaseBeginMultipartAttempted) != plan.Multipart || state.Rev != "" || state.UploadID != "" || len(state.Completed) != 0 {
			return false
		}
		return (state.DocID == "" && state.EditedRev == "") || (state.DocID != "" && state.EditedRev != "")
	case PhaseFinishAttempted:
		return ordinaryStateShapeValid(state, plan) && (!plan.Multipart || len(state.Completed) == plan.PartCount)
	default:
		return false
	}
}

func ordinaryStateValid(state State, plan Plan, target remote.UploadTarget) bool {
	valid := ordinaryStateShapeValid(state, plan)
	if target.Existing != nil {
		valid = valid && state.DocID == target.Existing.ID && state.EditedRev != "" && state.EditedRev == target.Existing.Rev
	} else {
		valid = valid && state.EditedRev == ""
	}
	return valid
}

func ordinaryStateShapeValid(state State, plan Plan) bool {
	valid := state.DocID != "" && state.Rev != ""
	if plan.Multipart {
		valid = valid && state.UploadID != ""
	} else {
		valid = valid && state.UploadID == "" && len(state.Completed) == 0
	}
	if valid {
		for part, completed := range state.Completed {
			if part < 1 || part > plan.PartCount || completed.ETag == "" || completed.Size != partLength(state.Fingerprint.Size, plan.PartSize, part) {
				return false
			}
		}
	}
	return valid
}

func pendingStateBaseValid(state State, server, normalized, abs string, fingerprint Fingerprint, plan Plan) bool {
	return state.Server == server && state.RemotePath == normalized && state.LocalPath == abs && state.Matches(fingerprint) &&
		state.PartSize == plan.PartSize && state.Completed != nil
}

func (u *Uploader) putSection(ctx context.Context, file *os.File, signed anyshare.SignedRequest, offset, length int64) (string, error) {
	body := io.NewSectionReader(file, offset, length)
	return u.API.PutSigned(ctx, signed, body, func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(file, offset, length)), nil
	}, length)
}

func validateRemoteItem(item anyshare.Item, normalized, name string, fingerprint Fingerprint, expectedID string) error {
	if itemIdentity(item) != expectedID || item.Type != "file" || item.Size != fingerprint.Size {
		return integrityError("remote identity or size mismatch")
	}
	if item.Path != "" && item.Path != normalized {
		return integrityError("remote path mismatch")
	}
	if item.Name != "" && item.Name != name {
		return integrityError("remote name mismatch")
	}
	if item.ClientMtimeUS != 0 && item.ClientMtimeUS != fingerprint.MtimeNS/1000 {
		return integrityError("remote modification time mismatch")
	}
	for _, pair := range [][2]string{{item.MD5, fingerprint.MD5}, {item.SliceMD5, fingerprint.SliceMD5}, {item.CRC32, fingerprint.CRC32}} {
		if pair[0] != "" && !strings.EqualFold(pair[0], pair[1]) {
			return integrityError("remote checksum mismatch")
		}
	}
	return nil
}

func validateResolvedItem(item anyshare.Item, normalized, name string, fingerprint Fingerprint, expectedID string) error {
	if item.Path != normalized || item.Name != name {
		return integrityError("remote path or name mismatch")
	}
	return validateRemoteItem(item, normalized, name, fingerprint, expectedID)
}

func fingerprintChecksums(f Fingerprint) anyshare.Checksums {
	return anyshare.Checksums{MD5: f.MD5, SliceMD5: f.SliceMD5, CRC32: f.CRC32}
}
func itemIdentity(item anyshare.Item) string {
	if item.ID != "" {
		return item.ID
	}
	return item.DocID
}
func partLength(size, partSize int64, part int) int64 {
	offset := int64(part-1) * partSize
	if remain := size - offset; remain < partSize {
		return remain
	}
	return partSize
}
func (u *Uploader) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}
func (u *Uploader) wait(ctx context.Context, delay time.Duration) error {
	if u.sleep != nil {
		return u.sleep(ctx, delay)
	}
	return contextSleep(ctx, delay)
}

func contextSleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return uploadError(apperr.Interrupted, "wait to reconcile upload", ctx.Err())
	}
}

func resultFromItem(item anyshare.Item, path string, size int64, instant, multipart bool, total, resumed, uploaded int, didResume bool) Result {
	return Result{RemotePath: path, RemoteID: itemIdentity(item), Revision: item.Rev, Size: size, Instant: instant, Multipart: multipart,
		PartsTotal: total, PartsResumed: resumed, PartsUploaded: uploaded, Resumed: didResume}
}

func ambiguousUploadError(err error) bool {
	if anyshare.IsRequestNotSent(err) {
		return false
	}
	if anyshare.IsOutcomeUnknown(err) {
		return true
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr != nil {
		return appErr.Category == apperr.Network || appErr.Category == apperr.Interrupted
	}
	return false
}

func errorCategory(err error) apperr.Category {
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr != nil {
		return appErr.Category
	}
	return ""
}

func interruptedUploadError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errorCategory(err) == apperr.Interrupted
}

func classifyUploadError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return uploadError(apperr.Interrupted, op, err)
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) && appErr != nil {
		return apperr.Wrap(appErr.Category, op, "upload failed", err)
	}
	return uploadError(apperr.Network, op, err)
}

func uploadError(category apperr.Category, op string, err error) error {
	return apperr.Wrap(category, op, "upload failed", err)
}
func integrityError(message string) error {
	return apperr.Wrap(apperr.Integrity, "verify upload", message, errors.New("remote upload differs from local file"))
}
