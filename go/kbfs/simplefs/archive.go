// Copyright 2024 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package simplefs

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"gopkg.in/src-d/go-billy.v4"
)

func loadArchiveStateFromJsonGz(ctx context.Context, simpleFS *SimpleFS, filePath string) (state *keybase1.SimpleFSArchiveState, err error) {
	f, err := os.Open(filePath)
	if err != nil {
		simpleFS.log.CErrorf(ctx, "loadArchiveStateFromJsonGz: opening state file error: %v", err)
		return nil, err
	}
	defer f.Close()
	gzReader, err := gzip.NewReader(f)
	if err != nil {
		simpleFS.log.CErrorf(ctx, "loadArchiveStateFromJsonGz: creating gzip reader error: %v", err)
		return nil, err
	}
	decoder := json.NewDecoder(gzReader)
	err = decoder.Decode(&state)
	if err != nil {
		simpleFS.log.CErrorf(ctx, "loadArchiveStateFromJsonGz: decoding state file error: %v", err)
		return nil, err
	}
	return state, nil
}

func writeArchiveStateIntoJsonGz(ctx context.Context, simpleFS *SimpleFS, filePath string, s *keybase1.SimpleFSArchiveState) error {
	err := os.MkdirAll(filepath.Dir(filePath), 0755)
	if err != nil {
		simpleFS.log.CErrorf(ctx, "writeArchiveStateIntoJsonGz: os.MkdirAll error: %v", err)
		return err
	}
	f, err := os.Create(filePath)
	if err != nil {
		simpleFS.log.CErrorf(ctx, "writeArchiveStateIntoJsonGz: creating state file error: %v", err)
		return err
	}
	defer f.Close()

	gzWriter := gzip.NewWriter(f)
	defer gzWriter.Close()

	encoder := json.NewEncoder(gzWriter)
	err = encoder.Encode(s)
	if err != nil {
		simpleFS.log.CErrorf(ctx, "writeArchiveStateIntoJsonGz: encoding state file error: %v", err)
		return err
	}

	return nil
}

type errorState struct {
	err       error
	nextRetry time.Time
}

type archiveManager struct {
	simpleFS *SimpleFS

	// Just use a regular mutex rather than a rw one so all writes to
	// persistent storage are synchronized.
	mu               sync.Mutex
	state            *keybase1.SimpleFSArchiveState
	jobCtxCancellers map[string]func()
	// jobID -> errorState. Populated when an error has happened. It's only
	// valid for these phases:
	//
	//   keybase1.SimpleFSArchiveJobPhase_Indexing
	//   keybase1.SimpleFSArchiveJobPhase_Copying
	//   keybase1.SimpleFSArchiveJobPhase_Zipping
	//
	// When nextRetry is current errorRetryWorker delete the errorState from
	// this map, while also putting them back to the previous phase so the
	// worker can pick it up.
	errors map[string]errorState

	indexingWorkerSignal chan struct{}
	copyingWorkerSignal  chan struct{}
	zippingWorkerSignal  chan struct{}

	ctxCancel func()
}

func getStateFilePath(simpleFS *SimpleFS) string {
	username := simpleFS.config.KbEnv().GetUsername()
	cacheDir := simpleFS.getCacheDir()
	return filepath.Join(cacheDir, fmt.Sprintf("kbfs-archive-%s.json.gz", username))
}

func (m *archiveManager) flushStateFileLocked(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	err := writeArchiveStateIntoJsonGz(ctx, m.simpleFS, getStateFilePath(m.simpleFS), m.state)
	if err != nil {
		m.simpleFS.log.CErrorf(ctx,
			"archiveManager.flushStateFileLocked: writing state file error: %v", err)
		return err
	}
	return nil
}

func (m *archiveManager) flushStateFile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushStateFileLocked(ctx)
}

func (m *archiveManager) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
		// There's already a signal in the chan. Skipping this.
	}
}

func (m *archiveManager) shutdown(ctx context.Context) {
	// OK to cancel before flushStateFileLocked because we'll pass in the
	// shutdown ctx there.
	if m.ctxCancel != nil {
		m.ctxCancel()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.flushStateFileLocked(ctx)
	if err != nil {
		m.simpleFS.log.CWarningf(ctx, "m.flushStateFileLocked error: %v", err)
	}
}

func (m *archiveManager) startJob(ctx context.Context, job keybase1.SimpleFSArchiveJobDesc) error {
	m.simpleFS.log.CDebugf(ctx, "+ archiveManager.startJob %#+v", job)
	defer m.simpleFS.log.CDebugf(ctx, "- archiveManager.startJob")

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.state.Jobs[job.JobID]; ok {
		return errors.New("job ID already exists")
	}
	m.state.Jobs[job.JobID] = keybase1.SimpleFSArchiveJobState{
		Desc:  job,
		Phase: keybase1.SimpleFSArchiveJobPhase_Queued,
	}
	m.state.LastUpdated = keybase1.ToTime(time.Now())
	m.signal(m.indexingWorkerSignal)
	return m.flushStateFileLocked(ctx)
}

func (m *archiveManager) cancelOrDismissJob(ctx context.Context,
	jobID string) (err error) {
	m.simpleFS.log.CDebugf(ctx, "+ archiveManager.cancelOrDismissJob")
	defer m.simpleFS.log.CDebugf(ctx, "- archiveManager.cancelOrDismissJob %s", jobID)
	m.mu.Lock()
	defer m.mu.Unlock()

	if cancel, ok := m.jobCtxCancellers[jobID]; ok {
		cancel()
		delete(m.jobCtxCancellers, jobID)
	}

	job, ok := m.state.Jobs[jobID]
	if !ok {
		return errors.New("job not found")
	}
	delete(m.state.Jobs, jobID)

	err = os.RemoveAll(job.Desc.StagingPath)
	if err != nil {
		m.simpleFS.log.CWarningf(ctx, "removing staging path %q for job %s error: %v",
			job.Desc.StagingPath, jobID, err)
	}

	return nil
}

func (m *archiveManager) getCurrentState(ctx context.Context) (
	state keybase1.SimpleFSArchiveState, errorStates map[string]errorState) {
	m.simpleFS.log.CDebugf(ctx, "+ archiveManager.getCurrentState")
	defer m.simpleFS.log.CDebugf(ctx, "- archiveManager.getCurrentState")
	m.mu.Lock()
	defer m.mu.Unlock()
	errorStates = make(map[string]errorState)
	for jobID, errState := range m.errors {
		errorStates[jobID] = errState
	}
	return m.state.DeepCopy(), errorStates
}

func (m *archiveManager) changeJobPhaseLocked(ctx context.Context,
	jobID string, newPhase keybase1.SimpleFSArchiveJobPhase) {
	copy, ok := m.state.Jobs[jobID]
	if !ok {
		m.simpleFS.log.CWarningf(ctx, "job %s not found. it might have been canceled", jobID)
		return
	}
	copy.Phase = newPhase
	m.state.Jobs[jobID] = copy
}
func (m *archiveManager) changeJobPhase(ctx context.Context,
	jobID string, newPhase keybase1.SimpleFSArchiveJobPhase) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.changeJobPhaseLocked(ctx, jobID, newPhase)
}

func (m *archiveManager) startWorkerTask(ctx context.Context,
	eligiblePhase keybase1.SimpleFSArchiveJobPhase,
	newPhase keybase1.SimpleFSArchiveJobPhase) (jobID string, jobCtx context.Context, ok bool) {
	jobCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	for jobID := range m.state.Jobs {
		if m.state.Jobs[jobID].Phase == eligiblePhase {
			m.changeJobPhaseLocked(ctx, jobID, newPhase)
			m.jobCtxCancellers[jobID] = cancel
			return jobID, jobCtx, true
		}
	}
	return "", nil, false
}

const archiveErrorRetryDuration = time.Minute

func (m *archiveManager) setJobError(
	ctx context.Context, jobID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nextRetry := time.Now().Add(archiveErrorRetryDuration)
	m.simpleFS.log.CErrorf(ctx, "job %s nextRetry: %s", jobID, nextRetry)
	m.errors[jobID] = errorState{
		err:       err,
		nextRetry: nextRetry,
	}
}

func (m *archiveManager) doIndexing(ctx context.Context, jobID string) (err error) {
	m.simpleFS.log.CDebugf(ctx, "+ doIndexing %s", jobID)
	defer func() { m.simpleFS.log.CDebugf(ctx, "- doIndexing %s err: %v", jobID, err) }()

	jobDesc := func() keybase1.SimpleFSArchiveJobDesc {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.state.Jobs[jobID].Desc
	}()
	opid, err := m.simpleFS.SimpleFSMakeOpid(ctx)
	if err != nil {
		return err
	}
	defer m.simpleFS.SimpleFSClose(ctx, opid)
	filter := keybase1.ListFilter_NO_FILTER
	err = m.simpleFS.SimpleFSListRecursive(ctx, keybase1.SimpleFSListRecursiveArg{
		OpID:   opid,
		Path:   keybase1.NewPathWithKbfsArchived(jobDesc.KbfsPathWithRevision),
		Filter: filter,
	})
	err = m.simpleFS.SimpleFSWait(ctx, opid)
	if err != nil {
		return err
	}

	listResult, err := m.simpleFS.SimpleFSReadList(ctx, opid)
	if err != nil {
		return err
	}

	var bytesTotal int64
	manifest := make(map[string]keybase1.SimpleFSArchiveFile)
	for _, e := range listResult.Entries {
		manifest[e.Name] = keybase1.SimpleFSArchiveFile{
			State:      keybase1.SimpleFSFileArchiveState_ToDo,
			DirentType: e.DirentType,
		}
		if e.DirentType == keybase1.DirentType_FILE ||
			e.DirentType == keybase1.DirentType_EXEC {
			bytesTotal += int64(e.Size)
		}
	}

	func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		jobCopy, ok := m.state.Jobs[jobID]
		if !ok {
			m.simpleFS.log.CWarningf(ctx, "job %s not found. it might have been canceled", jobID)
			return
		}
		jobCopy.Manifest = manifest
		jobCopy.BytesTotal = bytesTotal
		m.state.Jobs[jobID] = jobCopy
	}()
	return nil
}

func (m *archiveManager) indexingWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.indexingWorkerSignal:
		}

		jobID, jobCtx, ok := m.startWorkerTask(ctx,
			keybase1.SimpleFSArchiveJobPhase_Queued,
			keybase1.SimpleFSArchiveJobPhase_Indexing)

		if !ok {
			continue
		}
		// We got a task. Put another token into the signal channel so we
		// check again on the next iteration.
		m.signal(m.indexingWorkerSignal)

		m.simpleFS.log.CDebugf(ctx, "indexing: %s", jobID)

		err := m.doIndexing(jobCtx, jobID)
		if err == nil {
			m.simpleFS.log.CDebugf(jobCtx, "indexing done on job %s", jobID)
			m.changeJobPhase(jobCtx, jobID, keybase1.SimpleFSArchiveJobPhase_Indexed)
			m.signal(m.copyingWorkerSignal) // Done indexing! Notify the copying worker.
		} else {
			m.simpleFS.log.CErrorf(jobCtx, "indexing error on job %s: %v", jobID, err)
			m.setJobError(ctx, jobID, err)
		}

		err = m.flushStateFile(ctx)
		if err != nil {
			m.simpleFS.log.CWarningf(ctx, "m.flushStateFileLocked error: %v", err)
		}
	}
}

type sha256TeeReader struct {
	inner          io.Reader
	innerTeeReader io.Reader
	h              hash.Hash
}

var _ io.Reader = (*sha256TeeReader)(nil)

// Read implements the io.Reader interface.
func (r *sha256TeeReader) Read(p []byte) (n int, err error) {
	return r.innerTeeReader.Read(p)
}

func (r *sha256TeeReader) getSum() []byte {
	return r.h.Sum(nil)
}

func newSHA256TeeReader(inner io.Reader) (r *sha256TeeReader) {
	r = &sha256TeeReader{
		inner: inner,
		h:     sha256.New(),
	}
	r.innerTeeReader = io.TeeReader(r.inner, r.h)
	return r
}

type bytesUpdaterFunc = func(delta int64)

func ctxAwareCopy(
	ctx context.Context, to io.Writer, from io.Reader,
	bytesUpdater bytesUpdaterFunc) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := io.CopyN(to, from, 64*1024)
		switch err {
		case nil:
			bytesUpdater(n)
		case io.EOF:
			bytesUpdater(n)
			return nil
		default:
			return err
		}
	}
}

func (m *archiveManager) copyFileFromBeginning(ctx context.Context,
	srcDirFS billy.Filesystem, entryPathWithinJob string,
	localPath string, mode os.FileMode,
	bytesCopiedUpdater bytesUpdaterFunc) (sha256Sum []byte, err error) {
	m.simpleFS.log.CDebugf(ctx, "+ copyFileFromBeginning %s", entryPathWithinJob)
	defer func() { m.simpleFS.log.CDebugf(ctx, "- copyFileFromBeginning %s err: %v", entryPathWithinJob, err) }()

	src, err := srcDirFS.Open(entryPathWithinJob)
	if err != nil {
		return nil, fmt.Errorf("srcDirFS.Open(%s) error: %v", entryPathWithinJob, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return nil, fmt.Errorf("os.OpenFile(%s) error: %v", localPath, err)
	}
	defer dst.Close()

	teeReader := newSHA256TeeReader(src)

	err = ctxAwareCopy(ctx, dst, teeReader, bytesCopiedUpdater)
	if err != nil {
		return nil, fmt.Errorf("[%s] io.CopyN error: %v", entryPathWithinJob, err)
	}

	// We didn't continue from a previously interrupted copy, so don't
	// bother verifying the sha256sum and just return it.
	return teeReader.getSum(), nil
}

func (m *archiveManager) copyFilePickupPrevious(ctx context.Context,
	srcDirFS billy.Filesystem, entryPathWithinJob string,
	localPath string, srcSeekOffset int64, mode os.FileMode,
	bytesCopiedUpdater bytesUpdaterFunc) (sha256Sum []byte, err error) {
	m.simpleFS.log.CDebugf(ctx, "+ copyFilePickupPrevious %s", entryPathWithinJob)
	defer func() { m.simpleFS.log.CDebugf(ctx, "- copyFilePickupPrevious %s err: %v", entryPathWithinJob, err) }()

	src, err := srcDirFS.Open(entryPathWithinJob)
	if err != nil {
		return nil, fmt.Errorf("srcDirFS.Open(%s) error: %v", entryPathWithinJob, err)
	}
	defer src.Close()

	_, err = src.Seek(srcSeekOffset, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("[%s] src.Seek error: %v", entryPathWithinJob, err)
	}

	// Copy the file.
	if err = func() error {
		dst, err := os.OpenFile(localPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, mode)
		if err != nil {
			return fmt.Errorf("os.OpenFile(%s) error: %v", localPath, err)
		}
		defer dst.Close()

		err = ctxAwareCopy(ctx, dst, src, bytesCopiedUpdater)
		if err != nil {
			return fmt.Errorf("[%s] io.CopyN error: %v", entryPathWithinJob, err)
		}

		return nil
	}(); err != nil {
		return nil, err
	}

	var size int64
	// Calculate sha256 and check the sha256 of the copied file since we
	// continued from a previously interrupted copy.
	srcSHA256Sum, dstSHA256Sum, err := func() (srcSHA256Sum, dstSHA256Sum []byte, err error) {
		_, err = src.Seek(0, io.SeekStart)
		if err != nil {
			return nil, nil, fmt.Errorf("[%s] src.Seek error: %v", entryPathWithinJob, err)
		}
		srcSHA256SumHasher := sha256.New()
		size, err = io.Copy(srcSHA256SumHasher, src)
		if err != nil {
			return nil, nil, fmt.Errorf("[%s] io.Copy error: %v", entryPathWithinJob, err)
		}
		srcSHA256Sum = srcSHA256SumHasher.Sum(nil)

		dst, err := os.Open(localPath)
		if err != nil {
			return nil, nil, fmt.Errorf("os.Open(%s) error: %v", localPath, err)
		}
		defer dst.Close()
		dstSHA256SumHasher := sha256.New()
		_, err = io.Copy(dstSHA256SumHasher, dst)
		if err != nil {
			return nil, nil, fmt.Errorf("[%s] io.Copy error: %v", entryPathWithinJob, err)
		}
		dstSHA256Sum = dstSHA256SumHasher.Sum(nil)

		return srcSHA256Sum, dstSHA256Sum, nil
	}()
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(srcSHA256Sum, dstSHA256Sum) {
		m.simpleFS.log.CInfof(ctx,
			"file corruption is detected from a previous copy. Will copy from the beginning: ",
			entryPathWithinJob)
		bytesCopiedUpdater(-size)
		return m.copyFileFromBeginning(ctx, srcDirFS, entryPathWithinJob, localPath, mode, bytesCopiedUpdater)
	}

	return srcSHA256Sum, nil
}

func (m *archiveManager) copyFile(ctx context.Context,
	srcDirFS billy.Filesystem, entryPathWithinJob string,
	localPath string, srcSeekOffset int64, mode os.FileMode,
	bytesCopiedUpdater bytesUpdaterFunc) (sha256Sum []byte, err error) {
	if srcSeekOffset == 0 {
		return m.copyFileFromBeginning(ctx, srcDirFS, entryPathWithinJob, localPath, mode, bytesCopiedUpdater)
	}
	return m.copyFilePickupPrevious(ctx, srcDirFS, entryPathWithinJob, localPath, srcSeekOffset, mode, bytesCopiedUpdater)
}

func getWorkspaceDir(jobDesc keybase1.SimpleFSArchiveJobDesc) string {
	return filepath.Join(jobDesc.StagingPath, "workspace")
}

func (m *archiveManager) doCopying(ctx context.Context, jobID string) (err error) {
	m.simpleFS.log.CDebugf(ctx, "+ doCopying %s", jobID)
	defer func() { m.simpleFS.log.CDebugf(ctx, "- doCopying %s err: %v", jobID, err) }()

	desc, manifest := func() (keybase1.SimpleFSArchiveJobDesc, map[string]keybase1.SimpleFSArchiveFile) {
		m.mu.Lock()
		defer m.mu.Unlock()
		manifest := make(map[string]keybase1.SimpleFSArchiveFile)
		for k, v := range m.state.Jobs[jobID].Manifest {
			manifest[k] = v.DeepCopy()
		}
		return m.state.Jobs[jobID].Desc, manifest
	}()

	updateManifest := func(manifest map[string]keybase1.SimpleFSArchiveFile) {
		m.mu.Lock()
		defer m.mu.Unlock()
		// Can override directly since only one worker can work on a give job at a time.
		job := m.state.Jobs[jobID]
		for k, v := range manifest {
			job.Manifest[k] = v.DeepCopy()
		}
		m.state.Jobs[jobID] = job
	}

	updateBytesCopied := func(delta int64) {
		m.mu.Lock()
		defer m.mu.Unlock()
		// Can override directly since only one worker can work on a give job at a time.
		job := m.state.Jobs[jobID]
		job.BytesCopied += delta
		m.state.Jobs[jobID] = job
	}

	srcContainingDirFS, finalElem, err := m.simpleFS.getFSIfExists(ctx,
		keybase1.NewPathWithKbfsArchived(desc.KbfsPathWithRevision))
	if err != nil {
		return fmt.Errorf("getFSIfExists error: %v", err)
	}
	srcDirFS, err := srcContainingDirFS.Chroot(finalElem)
	if err != nil {
		return fmt.Errorf("srcContainingDirFS.Chroot error: %v", err)
	}
	dstBase := filepath.Join(getWorkspaceDir(desc), desc.TargetName)

	entryPaths := make([]string, 0, len(manifest))
	for entryPathWithinJob := range manifest {
		entryPaths = append(entryPaths, entryPathWithinJob)
	}
	sort.Strings(entryPaths)

loopEntryPaths:
	for _, entryPathWithinJob := range entryPaths {
		entry := manifest[entryPathWithinJob]
		entry.State = keybase1.SimpleFSFileArchiveState_InProgress
		manifest[entryPathWithinJob] = entry
		updateManifest(manifest)

		localPath := filepath.Join(dstBase, entryPathWithinJob)
		srcFI, err := srcDirFS.Lstat(entryPathWithinJob)
		if err != nil {
			return fmt.Errorf("srcDirFS.LStat(%s) error: %v", entryPathWithinJob, err)
		}
		switch {
		case srcFI.IsDir():
			err = os.MkdirAll(localPath, 0755)
			if err != nil {
				return fmt.Errorf("os.MkdirAll(%s) error: %v", localPath, err)
			}
			err = os.Chtimes(localPath, time.Time{}, srcFI.ModTime())
			if err != nil {
				return fmt.Errorf("os.Chtimes(%s) error: %v", localPath, err)
			}
			entry.State = keybase1.SimpleFSFileArchiveState_Complete
			manifest[entryPathWithinJob] = entry
		case srcFI.Mode()&os.ModeSymlink != 0: // symlink
			err = os.MkdirAll(filepath.Dir(localPath), 0755)
			if err != nil {
				return fmt.Errorf("os.MkdirAll(filepath.Dir(%s)) error: %v", localPath, err)
			}
			// Call Stat, which follows symlinks, to make sure the link doesn't
			// escape outside the srcDirFS.
			_, err = srcDirFS.Stat(entryPathWithinJob)
			if err != nil {
				m.simpleFS.log.CWarningf(ctx, "skipping %s due to srcDirFS.Stat error: %v", entryPathWithinJob, err)
				entry.State = keybase1.SimpleFSFileArchiveState_Skipped
				manifest[entryPathWithinJob] = entry
				continue loopEntryPaths
			}

			link, err := srcDirFS.Readlink(entryPathWithinJob)
			if err != nil {
				return fmt.Errorf("srcDirFS(%s) error: %v", entryPathWithinJob, err)
			}
			m.simpleFS.log.CInfof(ctx, "calling os.Symlink(%s, %s) ", link, localPath)
			err = os.Symlink(link, localPath)
			if err != nil {
				return fmt.Errorf("os.Symlink(%s, %s) error: %v", link, localPath, err)
			}
			// Skipping Chtimes becasue there doesn't seem to be a way to
			// change time on symlinks.
			entry.State = keybase1.SimpleFSFileArchiveState_Complete
			manifest[entryPathWithinJob] = entry
		default:
			err = os.MkdirAll(filepath.Dir(localPath), 0755)
			if err != nil {
				return fmt.Errorf("os.MkdirAll(filepath.Dir(%s)) error: %v", localPath, err)
			}

			var mode os.FileMode = 0644
			if srcFI.Mode()&0100 != 0 {
				mode = 0755
			}

			seek := int64(0)

			dstFI, err := os.Lstat(localPath)
			switch {
			case os.IsNotExist(err): // simple copy from the start of file
			case err == nil: // continue from a previously interrupted copy
				if srcFI.Mode()&os.ModeSymlink == 0 {
					seek = dstFI.Size()
				}
				// otherwise copy from the start of file
			default:
				return fmt.Errorf("os.Lstat(%s) error: %v", localPath, err)
			}

			sha256Sum, err := m.copyFile(ctx,
				srcDirFS, entryPathWithinJob, localPath, seek, mode, updateBytesCopied)
			if err != nil {
				return err
			}

			err = os.Chtimes(localPath, time.Time{}, srcFI.ModTime())
			if err != nil {
				return fmt.Errorf("os.Chtimes(%s) error: %v", localPath, err)
			}

			entry.Sha256SumHex = hex.EncodeToString(sha256Sum)
			entry.State = keybase1.SimpleFSFileArchiveState_Complete
			manifest[entryPathWithinJob] = entry
		}
		updateManifest(manifest)
	}

	return nil
}

func (m *archiveManager) copyingWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.copyingWorkerSignal:
		}

		jobID, jobCtx, ok := m.startWorkerTask(ctx,
			keybase1.SimpleFSArchiveJobPhase_Indexed,
			keybase1.SimpleFSArchiveJobPhase_Copying)

		if !ok {
			continue
		}
		// We got a task. Put another token into the signal channel so we
		// check again on the next iteration.
		m.signal(m.copyingWorkerSignal)

		m.simpleFS.log.CDebugf(ctx, "copying: %s", jobID)

		err := m.doCopying(jobCtx, jobID)
		if err == nil {
			m.simpleFS.log.CDebugf(jobCtx, "copying done on job %s", jobID)
			m.changeJobPhase(jobCtx, jobID, keybase1.SimpleFSArchiveJobPhase_Copied)
			m.signal(m.zippingWorkerSignal) // Done copying! Notify the zipping worker.
		} else {
			m.simpleFS.log.CErrorf(jobCtx, "copying error on job %s: %v", jobID, err)
			m.setJobError(ctx, jobID, err)
		}

		err = m.flushStateFile(ctx)
		if err != nil {
			m.simpleFS.log.CWarningf(ctx, "m.flushStateFileLocked error: %v", err)
		}
	}
}

// zipWriterAddDir is adapted from zip.Writer.AddFS in go1.22.0 source because 1) we're
// not on a version with this function yet, and 2) Go's AddFS doesn't support
// symlinks; 3) we need bytesZippedUpdater here and we need to use CopyN for it.
func zipWriterAddDir(ctx context.Context,
	w *zip.Writer, dirPath string, bytesZippedUpdater bytesUpdaterFunc) error {
	fsys := os.DirFS(dirPath)
	return fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !(info.Mode() &^ fs.ModeSymlink).IsRegular() {
			return errors.New("zip: cannot add non-regular file except symlink")
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = name
		h.Method = zip.Deflate
		fw, err := w.CreateHeader(h)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(filepath.Join(dirPath, name))
			if err != nil {
				return err
			}
			_, err = fw.Write([]byte(filepath.ToSlash(target)))
			if err != nil {
				return err
			}
			return nil
		default:
			f, err := fsys.Open(name)
			if err != nil {
				return err
			}
			defer f.Close()
			ctxAwareCopy(ctx, fw, f, bytesZippedUpdater)
			return nil
		}
	})
}

func (m *archiveManager) doZipping(ctx context.Context, jobID string) (err error) {
	m.simpleFS.log.CDebugf(ctx, "+ doZipping %s", jobID)
	defer func() { m.simpleFS.log.CDebugf(ctx, "- doZipping %s err: %v", jobID, err) }()

	jobDesc := func() keybase1.SimpleFSArchiveJobDesc {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.state.Jobs[jobID].Desc
	}()

	// Reset BytesZipped.
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		// Can override directly since only one worker can work on a give job at a time.
		job := m.state.Jobs[jobID]
		job.BytesZipped = 0
		m.state.Jobs[jobID] = job
	}()

	updateBytesZipped := func(delta int64) {
		m.mu.Lock()
		defer m.mu.Unlock()
		// Can override directly since only one worker can work on a give job at a time.
		job := m.state.Jobs[jobID]
		job.BytesZipped += delta
		m.state.Jobs[jobID] = job
	}

	workspaceDir := getWorkspaceDir(jobDesc)

	err = func() (err error) {
		mode := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if jobDesc.OverwriteZip {
			mode = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		zipFile, err := os.OpenFile(jobDesc.ZipFilePath, mode, 0666)
		if err != nil {
			return fmt.Errorf("os.Create(%s) error: %v", jobDesc.ZipFilePath, err)
		}
		defer func() {
			closeErr := zipFile.Close()
			if err == nil {
				err = closeErr
			}
		}()

		zipWriter := zip.NewWriter(zipFile)
		defer func() {
			closeErr := zipWriter.Close()
			if err == nil {
				err = closeErr
			}
		}()

		err = zipWriterAddDir(ctx, zipWriter, workspaceDir, updateBytesZipped)
		if err != nil {
			return fmt.Errorf("zipWriter.AddFS to %s error: %v", jobDesc.ZipFilePath, err)
		}

		return nil
	}()
	if err != nil {
		return err
	}

	// Remove the workspace so we release the storage space early on before
	// user dismisses the job.
	err = os.RemoveAll(workspaceDir)
	if err != nil {
		m.simpleFS.log.CWarningf(ctx, "removing workspace %s error %v", workspaceDir, err)
	}

	return nil
}

func (m *archiveManager) zippingWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.zippingWorkerSignal:
		}

		jobID, jobCtx, ok := m.startWorkerTask(ctx,
			keybase1.SimpleFSArchiveJobPhase_Copied,
			keybase1.SimpleFSArchiveJobPhase_Zipping)

		if !ok {
			continue
		}
		// We got a task. Put another token into the signal channel so we
		// check again on the next iteration.
		m.signal(m.zippingWorkerSignal)

		m.simpleFS.log.CDebugf(ctx, "zipping: %s", jobID)

		err := m.doZipping(jobCtx, jobID)
		if err == nil {
			m.simpleFS.log.CDebugf(jobCtx, "zipping done on job %s", jobID)
			m.changeJobPhase(jobCtx, jobID, keybase1.SimpleFSArchiveJobPhase_Done)
		} else {
			m.simpleFS.log.CErrorf(jobCtx, "zipping error on job %s: %v", jobID, err)
			m.setJobError(ctx, jobID, err)
		}

		err = m.flushStateFile(ctx)
		if err != nil {
			m.simpleFS.log.CWarningf(ctx, "m.flushStateFileLocked error: %v", err)
		}
	}
}

func (m *archiveManager) resetInterruptedPhaseLocked(ctx context.Context, jobID string) (changed bool) {
	switch m.state.Jobs[jobID].Phase {
	case keybase1.SimpleFSArchiveJobPhase_Indexing:
		m.simpleFS.log.CDebugf(ctx, "resetting %s phase from %s to %s", jobID,
			keybase1.SimpleFSArchiveJobPhase_Indexing,
			keybase1.SimpleFSArchiveJobPhase_Queued)
		m.changeJobPhaseLocked(ctx, jobID,
			keybase1.SimpleFSArchiveJobPhase_Queued)
		return true
	case keybase1.SimpleFSArchiveJobPhase_Copying:
		m.simpleFS.log.CDebugf(ctx, "resetting %s phase from %s to %s", jobID,
			keybase1.SimpleFSArchiveJobPhase_Copying,
			keybase1.SimpleFSArchiveJobPhase_Indexed)
		m.changeJobPhaseLocked(ctx, jobID,
			keybase1.SimpleFSArchiveJobPhase_Indexed)
		return true
	case keybase1.SimpleFSArchiveJobPhase_Zipping:
		m.simpleFS.log.CDebugf(ctx, "resetting %s phase from %s to %s", jobID,
			keybase1.SimpleFSArchiveJobPhase_Zipping,
			keybase1.SimpleFSArchiveJobPhase_Copied)
		m.changeJobPhaseLocked(ctx, jobID,
			keybase1.SimpleFSArchiveJobPhase_Copied)
		return true
	default:
		m.simpleFS.log.CDebugf(ctx, "not resetting %s phase from %s", jobID,
			m.state.Jobs[jobID].Phase)
		return false
	}
}

func (m *archiveManager) errorRetryWorker(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 5)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			jobIDs := make([]string, len(m.state.Jobs))
			for jobID := range m.state.Jobs {
				jobIDs = append(jobIDs, jobID)
			}
		loopJobIDs:
			for _, jobID := range jobIDs {
				errState, ok := m.errors[jobID]
				if !ok {
					continue loopJobIDs
				}
				if time.Now().Before(errState.nextRetry) {
					continue loopJobIDs
				}
				m.simpleFS.log.CDebugf(ctx, "retrying job %s", jobID)
				changed := m.resetInterruptedPhaseLocked(ctx, jobID)
				if !changed {
					m.simpleFS.log.CWarningf(ctx,
						"job %s has an error state %v but an unexpected job phase",
						jobID, errState.err)
					continue loopJobIDs
				}
				delete(m.errors, jobID)

				m.signal(m.indexingWorkerSignal)
				m.signal(m.copyingWorkerSignal)
				m.signal(m.zippingWorkerSignal)
			}
		}()
	}
}

func (m *archiveManager) start() {
	ctx := context.Background()
	ctx, m.ctxCancel = context.WithCancel(ctx)
	go m.indexingWorker(m.simpleFS.makeContext(ctx))
	go m.copyingWorker(m.simpleFS.makeContext(ctx))
	go m.zippingWorker(m.simpleFS.makeContext(ctx))
	go m.errorRetryWorker(m.simpleFS.makeContext(ctx))
	m.signal(m.indexingWorkerSignal)
	m.signal(m.copyingWorkerSignal)
	m.signal(m.zippingWorkerSignal)
}

func (m *archiveManager) resetInterruptedPhasesLocked(ctx context.Context) {
	// We don't resume indexing and zipping work, so just reset them here.
	// Copying is resumable but we have per file state tracking so reset the
	// phase here as well.
	for jobID := range m.state.Jobs {
		_ = m.resetInterruptedPhaseLocked(ctx, jobID)
	}
}

func newArchiveManager(simpleFS *SimpleFS) (m *archiveManager, err error) {
	ctx := context.Background()
	simpleFS.log.CDebugf(ctx, "+ newArchiveManager")
	defer simpleFS.log.CDebugf(ctx, "- newArchiveManager")
	m = &archiveManager{
		simpleFS:             simpleFS,
		jobCtxCancellers:     make(map[string]func()),
		errors:               make(map[string]errorState),
		indexingWorkerSignal: make(chan struct{}, 1),
		copyingWorkerSignal:  make(chan struct{}, 1),
		zippingWorkerSignal:  make(chan struct{}, 1),
	}
	stateFilePath := getStateFilePath(simpleFS)
	m.state, err = loadArchiveStateFromJsonGz(ctx, simpleFS, stateFilePath)
	switch err {
	case nil:
		if m.state.Jobs == nil {
			m.state.Jobs = make(map[string]keybase1.SimpleFSArchiveJobState)
		}
		m.resetInterruptedPhasesLocked(ctx)
	default:
		simpleFS.log.CErrorf(ctx, "loadArchiveStateFromJsonGz error ( %v ). Creating a new state.", err)
		m.state = &keybase1.SimpleFSArchiveState{
			Jobs: make(map[string]keybase1.SimpleFSArchiveJobState),
		}
		err = writeArchiveStateIntoJsonGz(ctx, simpleFS, stateFilePath, m.state)
		if err != nil {
			simpleFS.log.CErrorf(ctx, "newArchiveManager: creating state file error: %v", err)
			return nil, err
		}
	}
	m.start()
	return m, nil
}
