package chat

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/keybase/client/go/chat/attachments"
	"github.com/keybase/client/go/chat/globals"
	"github.com/keybase/client/go/chat/storage"
	"github.com/keybase/client/go/chat/types"
	"github.com/keybase/client/go/chat/utils"
	"github.com/keybase/client/go/chatrender"
	"github.com/keybase/client/go/encrypteddb"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/clockwork"
	"golang.org/x/sync/errgroup"
)

type ChatArchiveRegistry struct {
	globals.Contextified
	utils.DebugLabeler
	sync.Mutex

	started bool
	uid     gregor1.UID
	// Have we populated from disk?
	inited bool
	// Delay before we restart paused jobs on startup
	resumeJobsDelay time.Duration
	flushDelay      time.Duration
	stopCh          chan struct{}
	clock           clockwork.Clock
	eg              errgroup.Group
	// Changes to flush to disk?
	dirty        bool
	remoteClient func() chat1.RemoteInterface
	runningJobs  map[chat1.ArchiveJobID]types.CancelArchiveFn

	edb        *encrypteddb.EncryptedDB
	jobHistory chat1.ArchiveChatHistory
}

type ArchiveJobNotFoundError struct {
	jobID chat1.ArchiveJobID
}

func (e ArchiveJobNotFoundError) Error() string {
	return fmt.Sprintf("job not found: %s", e.jobID)
}

func NewArchiveJobNotFoundError(jobID chat1.ArchiveJobID) ArchiveJobNotFoundError {
	return ArchiveJobNotFoundError{jobID: jobID}
}

var _ error = ArchiveJobNotFoundError{}

func NewChatArchiveRegistry(g *globals.Context, remoteClient func() chat1.RemoteInterface) *ChatArchiveRegistry {
	keyFn := func(ctx context.Context) ([32]byte, error) {
		return storage.GetSecretBoxKey(ctx, g.ExternalG())
	}
	dbFn := func(g *libkb.GlobalContext) *libkb.JSONLocalDb {
		return g.LocalChatDb
	}
	r := &ChatArchiveRegistry{
		Contextified: globals.NewContextified(g),
		DebugLabeler: utils.NewDebugLabeler(g.ExternalG(), "ChatArchiveRegistry", false),
		remoteClient: remoteClient,
		clock:        clockwork.NewRealClock(),
		flushDelay:   15 * time.Second,
		runningJobs:  make(map[chat1.ArchiveJobID]types.CancelArchiveFn),
		jobHistory:   chat1.ArchiveChatHistory{JobHistory: make(map[chat1.ArchiveJobID]chat1.ArchiveChatJob)},
		edb:          encrypteddb.New(g.ExternalG(), dbFn, keyFn),
	}
	switch r.G().GetAppType() {
	case libkb.MobileAppType:
		r.resumeJobsDelay = 30 * time.Second
	default:
		r.resumeJobsDelay = 30 * time.Second
	}
	return r
}

func (r *ChatArchiveRegistry) dbKey() libkb.DbKey {
	version := 0
	key := fmt.Sprintf("ar:%d:%s", version, r.uid)
	return libkb.DbKey{
		Typ: libkb.DBChatArchiveRegistry,
		Key: key,
	}
}

func (r *ChatArchiveRegistry) initLocked(ctx context.Context) error {
	if !r.started {
		return errors.New("not started")
	}
	if r.inited {
		return nil
	}
	found, err := r.edb.Get(ctx, r.dbKey(), &r.jobHistory)
	if err != nil {
		return err
	}
	if !found {
		r.jobHistory = chat1.ArchiveChatHistory{JobHistory: make(map[chat1.ArchiveJobID]chat1.ArchiveChatJob)}
	}
	r.inited = true
	return nil
}

func (r *ChatArchiveRegistry) flushLocked(ctx context.Context) error {
	if r.dirty {
		err := r.edb.Put(ctx, r.dbKey(), r.jobHistory)
		if err != nil {
			return err
		}
		r.dirty = false
	}
	return nil
}

func (r *ChatArchiveRegistry) flushLoop(stopCh chan struct{}) error {
	ctx := context.Background()
	r.Debug(ctx, "flushLoop: starting")
	for {
		select {
		case <-stopCh:
			r.Debug(ctx, "flushLoop: shutting down")
			return nil
		case <-r.clock.After(r.flushDelay):
			func() {
				r.Lock()
				defer r.Unlock()
				err := r.flushLocked(ctx)
				if err != nil {
					r.Debug(ctx, "flushLoop: failed to flush: %s", err)
				}
			}()
		}
	}
}

func (r *ChatArchiveRegistry) resumeAllBgJobs(ctx context.Context) error {
	select {
	case <-r.stopCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.resumeJobsDelay):
	}
	r.Lock()
	defer r.Unlock()
	err := r.initLocked(ctx)
	if err != nil {
		return err
	}
	for _, job := range r.jobHistory.JobHistory {
		if job.Status == chat1.ArchiveChatJobStatus_BACKGROUND_PAUSED {
			go func(job chat1.ArchiveChatJob) {
				ctx := globals.ChatCtx(context.Background(), r.G(), keybase1.TLFIdentifyBehavior_CHAT_GUI, nil, NewSimpleIdentifyNotifier(r.G()))
				_, err := NewChatArchiver(r.G(), r.uid, r.remoteClient).ArchiveChat(ctx, job.Request)
				if err != nil {
					r.Debug(ctx, err.Error())
				}
			}(job)
		}
	}
	return nil
}

func (r *ChatArchiveRegistry) monitorAppState() error {
	appState := keybase1.MobileAppState_FOREGROUND
	ctx, cancel := context.WithCancel(context.Background())
	for {
		select {
		case <-r.stopCh:
			cancel()
			return nil
		case appState = <-r.G().MobileAppState.NextUpdate(&appState):
			switch appState {
			case keybase1.MobileAppState_FOREGROUND:
				go func() {
					ierr := r.resumeAllBgJobs(ctx)
					if ierr != nil {
						r.Debug(ctx, ierr.Error())
					}
				}()
			default:
				cancel()
				ctx, cancel = context.WithCancel(context.Background())
				r.bgPauseAllJobsLocked(ctx)
			}
		}
	}
}

// Resumes previously BACKGROUND_PAUSED jobs, after a delay.
func (r *ChatArchiveRegistry) Start(ctx context.Context, uid gregor1.UID) {
	defer r.Trace(ctx, nil, "Start")()
	r.Lock()
	defer r.Unlock()
	if r.started {
		return
	}
	r.uid = uid
	r.started = true
	r.stopCh = make(chan struct{})
	r.eg.Go(func() error {
		return r.flushLoop(r.stopCh)
	})
	r.eg.Go(func() error {
		return r.resumeAllBgJobs(context.Background())
	})
	r.eg.Go(r.monitorAppState)
}

func (r *ChatArchiveRegistry) bgPauseAllJobsLocked(ctx context.Context) {
	for jobID, cancel := range r.runningJobs {
		job := cancel()
		job.Status = chat1.ArchiveChatJobStatus_BACKGROUND_PAUSED
		r.jobHistory.JobHistory[jobID] = job
	}
	r.runningJobs = make(map[chat1.ArchiveJobID]func() chat1.ArchiveChatJob)

	r.dirty = true
	_ = r.flushLocked(ctx)
}

// Pause running jobs marking as BACKGROUND_PAUSED
func (r *ChatArchiveRegistry) Stop(ctx context.Context) chan struct{} {
	defer r.Trace(ctx, nil, "Stop")()
	r.Lock()
	defer r.Unlock()
	ch := make(chan struct{})
	if r.started {
		r.started = false
		r.bgPauseAllJobsLocked(ctx)
		close(r.stopCh)
		go func() {
			r.Debug(context.Background(), "Stop: waiting for shutdown")
			_ = r.eg.Wait()
			r.Debug(context.Background(), "Stop: shutdown complete")
			close(ch)
		}()
	} else {
		close(ch)
	}
	return ch

}

func (r *ChatArchiveRegistry) OnDbNuke(mctx libkb.MetaContext) (err error) {
	defer r.Trace(mctx.Ctx(), &err, "ChatArchiveRegistry.OnDbNuke")()
	r.Lock()
	defer r.Unlock()
	if !r.started {
		return nil
	}
	r.inited = false
	return nil
}

type ByJobStartedAt []chat1.ArchiveChatJob

func (c ByJobStartedAt) Len() int      { return len(c) }
func (c ByJobStartedAt) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c ByJobStartedAt) Less(i, j int) bool {
	x := c[i]
	y := c[j]
	if x.StartedAt == y.StartedAt {
		return x.Request.JobID < y.Request.JobID
	}
	return c[i].StartedAt.Before(c[j].StartedAt)
}

func (r *ChatArchiveRegistry) List(ctx context.Context) (res chat1.ArchiveChatListRes, err error) {
	defer r.Trace(ctx, &err, "List")()
	r.Lock()
	defer r.Unlock()
	err = r.initLocked(ctx)
	if err != nil {
		return res, err
	}

	for _, job := range r.jobHistory.JobHistory {
		res.Jobs = append(res.Jobs, job)
	}
	sort.Sort(ByJobStartedAt(res.Jobs))
	return res, nil
}

func (r *ChatArchiveRegistry) Get(ctx context.Context, jobID chat1.ArchiveJobID) (res chat1.ArchiveChatJob, err error) {
	defer r.Trace(ctx, &err, "Get(%s)", jobID)()
	r.Lock()
	defer r.Unlock()
	err = r.initLocked(ctx)
	if err != nil {
		return res, err
	}

	job, ok := r.jobHistory.JobHistory[jobID]
	if !ok {
		return res, NewArchiveJobNotFoundError(jobID)
	}
	return job, nil
}

func (r *ChatArchiveRegistry) Delete(ctx context.Context, jobID chat1.ArchiveJobID, deleteOutputPath bool) (err error) {
	defer r.Trace(ctx, &err, "Delete(%s)", jobID)()
	r.Lock()
	defer r.Unlock()
	err = r.initLocked(ctx)
	if err != nil {
		return err
	}

	cancel, ok := r.runningJobs[jobID]
	if ok {
		// Ignore the job output since we're deleting it anyway
		cancel()
		delete(r.runningJobs, jobID)
	}
	job, ok := r.jobHistory.JobHistory[jobID]
	if !ok {
		return NewArchiveJobNotFoundError(jobID)
	}
	delete(r.jobHistory.JobHistory, jobID)
	r.dirty = true
	if deleteOutputPath {
		err = os.RemoveAll(job.Request.OutputPath)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ChatArchiveRegistry) Set(ctx context.Context, cancel types.CancelArchiveFn, job chat1.ArchiveChatJob) (err error) {
	defer r.Trace(ctx, &err, "Set(%+v)", job)()
	r.Lock()
	defer r.Unlock()
	err = r.initLocked(ctx)
	if err != nil {
		return err
	}

	jobID := job.Request.JobID
	switch job.Status {
	case chat1.ArchiveChatJobStatus_COMPLETE:
		fallthrough
	case chat1.ArchiveChatJobStatus_ERROR:
		delete(r.runningJobs, jobID)
	case chat1.ArchiveChatJobStatus_RUNNING:
		if cancel != nil {
			r.runningJobs[jobID] = cancel
		}
	}

	r.jobHistory.JobHistory[jobID] = job
	r.dirty = true
	return nil
}

func (r *ChatArchiveRegistry) Pause(ctx context.Context, jobID chat1.ArchiveJobID) (err error) {
	defer r.Trace(ctx, &err, "Pause(%v)", jobID)()
	r.Lock()
	defer r.Unlock()

	err = r.initLocked(ctx)
	if err != nil {
		return err
	}

	job, ok := r.jobHistory.JobHistory[jobID]
	if !ok {
		return NewArchiveJobNotFoundError(jobID)
	}

	if job.Status != chat1.ArchiveChatJobStatus_RUNNING {
		return fmt.Errorf("Cannot pause a non-running job. Found status %v", job.Status)
	}

	cancel, ok := r.runningJobs[jobID]
	if !ok {
		return NewArchiveJobNotFoundError(jobID)
	}
	if cancel == nil {
		return fmt.Errorf("cancel unexpectedly nil")
	}
	delete(r.runningJobs, jobID)

	job = cancel()
	job.Status = chat1.ArchiveChatJobStatus_PAUSED
	r.jobHistory.JobHistory[jobID] = job
	r.dirty = true
	return nil
}

func (r *ChatArchiveRegistry) Resume(ctx context.Context, jobID chat1.ArchiveJobID) (err error) {
	defer r.Trace(ctx, &err, "Resume(%v)", jobID)()
	r.Lock()
	defer r.Unlock()

	err = r.initLocked(ctx)
	if err != nil {
		return err
	}

	job, ok := r.jobHistory.JobHistory[jobID]
	if !ok {
		return NewArchiveJobNotFoundError(jobID)
	}

	switch job.Status {
	case chat1.ArchiveChatJobStatus_ERROR:
	case chat1.ArchiveChatJobStatus_PAUSED:
	case chat1.ArchiveChatJobStatus_BACKGROUND_PAUSED:
	default:
		return fmt.Errorf("Cannot resume a non-paused job. Found status %v", job.Status)
	}

	// Resume the job in the background, the job will register itself as running
	go func() {
		_, err := NewChatArchiver(r.G(), r.uid, r.remoteClient).ArchiveChat(context.Background(), job.Request)
		if err != nil {
			r.Debug(ctx, err.Error())
		}
	}()
	return nil
}

var _ types.ChatArchiveRegistry = (*ChatArchiveRegistry)(nil)

const defaultPageSizeDesktop = 999
const defaultPageSizeMobile = 300

// Fullfil an archive query
type ChatArchiver struct {
	globals.Contextified
	utils.DebugLabeler
	uid gregor1.UID

	pageSize int

	sync.Mutex
	messagesComplete int64
	messagesTotal    int64
	remoteClient     func() chat1.RemoteInterface
}

func NewChatArchiver(g *globals.Context, uid gregor1.UID, remoteClient func() chat1.RemoteInterface) *ChatArchiver {
	c := &ChatArchiver{
		Contextified: globals.NewContextified(g),
		DebugLabeler: utils.NewDebugLabeler(g.ExternalG(), "ChatArchiver", false),
		uid:          uid,
		remoteClient: remoteClient,
	}
	switch c.G().GetAppType() {
	case libkb.MobileAppType:
		c.pageSize = defaultPageSizeMobile
	default:
		c.pageSize = defaultPageSizeDesktop
	}
	return c
}

func (c *ChatArchiver) notifyProgress(ctx context.Context, jobID chat1.ArchiveJobID, pagination chat1.Pagination) {
	c.Lock()
	defer c.Unlock()
	c.messagesComplete += int64(pagination.Num)
	if c.messagesComplete > c.messagesTotal || pagination.Last {
		// total messages is capped to the convs expunge, don't over report.
		c.messagesComplete = c.messagesTotal
	}
	c.G().NotifyRouter.HandleChatArchiveProgress(ctx, jobID, c.messagesComplete, c.messagesTotal)
}

func (c *ChatArchiver) archiveName(conv chat1.ConversationLocal) string {
	return chatrender.ConvName(c.G().GlobalContext, conv, c.G().GlobalContext.Env.GetUsername().String())
}

func (c *ChatArchiver) attachmentName(msg chat1.MessageUnboxedValid) string {
	body := msg.MessageBody
	typ, err := body.MessageType()
	if err != nil {
		return ""
	}
	if typ == chat1.MessageType_ATTACHMENT {
		att := body.Attachment()
		return fmt.Sprintf("%s (%d) - %s", gregor1.FromTime(msg.ServerHeader.Ctime).Format("2006-01-02 15.04.05"), msg.ServerHeader.MessageID, att.Object.Filename)
	}
	return ""
}

func (c *ChatArchiver) checkpointConv(ctx context.Context, f *os.File, cp chat1.ArchiveChatConvCheckpoint, convID chat1.ConversationID, job *chat1.ArchiveChatJob) (err error) {
	// Flush and update the registry
	err = f.Sync()
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	cp.Offset = stat.Size()

	c.Lock()
	// Mark our overall progress.
	job.MessagesTotal = c.messagesTotal
	job.MessagesComplete = c.messagesComplete
	// And this conv's individual progress.
	job.Checkpoints[convID.DbShortFormString()] = cp
	c.Unlock()

	return c.G().ArchiveRegistry.Set(ctx, nil, *job)
}

func (c *ChatArchiver) archiveConv(ctx context.Context, job *chat1.ArchiveChatJob, conv chat1.ConversationLocal) error {
	c.Lock()
	cp, ok := job.Checkpoints[conv.Info.Id.DbShortFormString()]
	c.Unlock()
	if !ok {
		cp = chat1.ArchiveChatConvCheckpoint{
			Pagination: chat1.Pagination{Num: c.pageSize},
			Offset:     0,
		}
	}

	convArchivePath := path.Join(job.Request.OutputPath, c.archiveName(conv), "chat.txt")
	f, err := os.OpenFile(convArchivePath, os.O_RDWR|os.O_CREATE, libkb.PermFile)
	if err != nil {
		return err
	}
	err = f.Truncate(cp.Offset)
	if err != nil {
		return err
	}
	_, err = f.Seek(cp.Offset, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	firstPage := cp.Offset == 0
	for !cp.Pagination.Last {
		thread, err := c.G().ConvSource.Pull(ctx, conv.Info.Id, c.uid,
			chat1.GetThreadReason_ARCHIVE, nil,
			&chat1.GetThreadQuery{
				MarkAsRead: false,
			}, &cp.Pagination)
		if err != nil {
			return err
		}

		msgs := thread.Messages

		// reverse the thread in place so we render in descending order in the file.
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}

		view := chatrender.ConversationView{
			Conversation: conv,
			Messages:     msgs,
			Opts: chatrender.RenderOptions{
				UseDateTime: true,
				// Only show the headline message once
				SkipHeadline: !firstPage,
			},
		}

		err = view.RenderToWriter(c.G().GlobalContext, f, 1024, false)
		if err != nil {
			return err
		}

		// Check for any attachment messages and download them alongside the chat.
		var eg errgroup.Group
		// Fetch attachments in parallel but limit the number since we
		// also allow parallel conv fetching.
		eg.SetLimit(5)
		for _, m := range msgs {
			if !m.IsValidFull() {
				continue
			}
			msg := m.Valid()
			body := msg.MessageBody
			typ, err := body.MessageType()
			if err != nil {
				return err
			}
			if typ == chat1.MessageType_ATTACHMENT {
				eg.Go(func() error {
					attachmentPath := path.Join(job.Request.OutputPath, c.archiveName(conv), c.attachmentName(msg))
					f, err := os.Create(attachmentPath)
					if err != nil {
						return err
					}
					defer f.Close()

					err = attachments.Download(ctx, c.G(), c.uid, conv.Info.Id,
						msg.ServerHeader.MessageID, f, false, func(_, _ int64) {}, c.remoteClient)
					if err != nil {
						return err
					}
					return nil
				})
			}
		}
		err = eg.Wait()
		if err != nil {
			return err
		}

		// update our progress percentage in the UI
		c.notifyProgress(ctx, job.Request.JobID, *thread.Pagination)

		// update our pagination so we can correctly fetch the next page and marking progress in our checkpoint.
		firstPage = false
		cp.Pagination = *thread.Pagination
		cp.Pagination.Num = c.pageSize
		cp.Pagination.Previous = nil
		ierr := c.checkpointConv(ctx, f, cp, conv.Info.Id, job)
		if ierr != nil {
			c.Debug(ctx, ierr.Error())
		}
	}
	return nil
}

func (c *ChatArchiver) ArchiveChat(ctx context.Context, arg chat1.ArchiveChatJobRequest) (outpath string, err error) {
	defer c.Trace(ctx, &err, "ArchiveChat")()

	if len(arg.OutputPath) == 0 {
		arg.OutputPath = path.Join(c.G().GlobalContext.Env.GetDownloadsDir(), fmt.Sprintf("kbchat-%s", arg.JobID))
	}

	// Make sure the root output path exists
	err = os.MkdirAll(arg.OutputPath, os.ModePerm)
	if err != nil {
		return "", err
	}

	// Resolve query to a set of convIDs.
	iboxRes, _, err := c.G().InboxSource.Read(ctx, c.uid, types.ConversationLocalizerBlocking,
		types.InboxSourceDataSourceAll, nil, arg.Query)
	if err != nil {
		return "", err
	}
	convs := iboxRes.Convs

	// Fetch size of each conv to track progress.
	for _, conv := range convs {
		c.messagesTotal += int64(conv.MaxVisibleMsgID() - conv.GetMaxDeletedUpTo())

		convArchivePath := path.Join(arg.OutputPath, c.archiveName(conv))
		err = os.MkdirAll(convArchivePath, os.ModePerm)
		if err != nil {
			return "", err
		}
	}

	jobInfo, err := c.G().ArchiveRegistry.Get(ctx, arg.JobID)
	if err != nil {
		if _, ok := err.(ArchiveJobNotFoundError); !ok {
			return "", err
		}
		jobInfo = chat1.ArchiveChatJob{
			Request:     arg,
			StartedAt:   gregor1.ToTime(time.Now()),
			Checkpoints: make(map[string]chat1.ArchiveChatConvCheckpoint),
		}
	}
	// Presume to resume
	jobInfo.Status = chat1.ArchiveChatJobStatus_RUNNING
	jobInfo.Err = ""

	// Setup to run each conv in parallel
	eg, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	// Closed if we are canceled
	cancelCh := make(chan struct{})
	doneCh := make(chan struct{})
	pause := func() chat1.ArchiveChatJob {
		cancel()
		close(cancelCh)
		// Block until we cleanup
		<-doneCh
		c.Lock()
		defer c.Unlock()
		return jobInfo
	}

	// Mark ourselves as running
	err = c.G().ArchiveRegistry.Set(ctx, pause, jobInfo)
	if err != nil {
		return "", err
	}
	// And update our state when we exit
	defer func() {
		defer func() { close(doneCh) }()
		select {
		case <-cancelCh:
			c.Debug(ctx, "canceled by registry, short-circuiting.")
			// If we were canceled by the registry, abort.
			return
		default:
		}

		// Update the registry
		jobInfo.Status = chat1.ArchiveChatJobStatus_COMPLETE
		if err != nil {
			jobInfo.Status = chat1.ArchiveChatJobStatus_ERROR
			jobInfo.Err = err.Error()
		}
		ierr := c.G().ArchiveRegistry.Set(ctx, nil, jobInfo)
		if ierr != nil {
			c.Debug(ctx, ierr.Error())
		}

		// Alert the UI
		c.G().NotifyRouter.HandleChatArchiveComplete(ctx, arg.JobID)
	}()

	// For each conv, fetch batches of messages until all are fetched.
	//    - Messages are rendered in a text format and attachments are downloaded to the archive path.
	eg.SetLimit(10)
	for _, conv := range convs {
		conv := conv
		eg.Go(func() error {
			return c.archiveConv(ctx, &jobInfo, conv)
		})
	}
	err = eg.Wait()
	if err != nil {
		return "", err
	}

	outpath = arg.OutputPath
	if arg.Compress {
		outpath += ".tar.gzip"
		err = tarGzip(arg.OutputPath, outpath)
		if err != nil {
			return "", err
		}
		err = os.RemoveAll(arg.OutputPath)
		if err != nil {
			return "", err
		}
	}

	return outpath, nil
}

func tarGzip(inPath, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zr := gzip.NewWriter(f)
	defer zr.Close()
	tw := tar.NewWriter(zr)
	defer tw.Close()

	err = filepath.Walk(inPath, func(fp string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(fi, fp)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(inPath, filepath.ToSlash(fp))
		if err != nil {
			return err
		}
		header.Name = name

		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		file, err := os.Open(fp)
		if err != nil {
			return err
		}
		defer file.Close()
		if _, err := io.Copy(tw, file); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}
