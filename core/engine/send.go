package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	maxUploadBytes = 25 * 1024 * 1024
	// sendConfirmTimeout bounds the wait for the server to confirm a written message; after it the
	// result is unknown and the message is never sent again by the core.
	sendConfirmTimeout = 30 * time.Second
	// sendReadyTimeout bounds the wait for the socket before anything is written (handlematrix.go:166).
	sendReadyTimeout = 15 * time.Second
	uploadTimeout    = 2 * time.Minute
	markReadTimeout  = 15 * time.Second

	otidRegistryCapacity = 4096
)

// otidRegistry keeps the OTID given to each client_id. Swift keeps messages in memory only, so a
// client_id never outlives this process and the registry is enough to make retries reuse the ID.
var otidRegistry = struct {
	sync.Mutex
	byClient map[string]int64
	used     map[int64]bool
	order    []string
}{byClient: map[string]int64{}, used: map[int64]bool{}}

// otidFromClientID returns the offline threading ID for a client_id: the same value for every send
// of that client_id, so the server deduplicates a retry. It has the shape of methods.GenerateEpochID
// (milliseconds << 22, as every Meta client and mautrix's GenerateTransactionID produce), with the
// low 22 bits taken from a hash of the client_id. A pure 63-bit hash would put an arbitrary date in
// the timestamp bits that Meta reads from OTIDs and message IDs.
func otidFromClientID(clientID string) int64 {
	otidRegistry.Lock()
	defer otidRegistry.Unlock()
	if otid, ok := otidRegistry.byClient[clientID]; ok {
		return otid
	}
	sum := sha256.Sum256([]byte(clientID))
	otid := time.Now().UnixMilli()<<22 | int64(binary.BigEndian.Uint32(sum[:4])&(1<<22-1))
	for otidRegistry.used[otid] {
		otid++
	}
	otidRegistry.byClient[clientID] = otid
	otidRegistry.used[otid] = true
	otidRegistry.order = append(otidRegistry.order, clientID)
	if len(otidRegistry.order) > otidRegistryCapacity {
		oldest := otidRegistry.order[0]
		otidRegistry.order = otidRegistry.order[1:]
		delete(otidRegistry.used, otidRegistry.byClient[oldest])
		delete(otidRegistry.byClient, oldest)
	}
	return otid
}

// waMessageID is the E2EE message ID for a client_id: the decimal OTID, as handlematrix.go:97 does.
func waMessageID(clientID string) string {
	return strconv.FormatInt(otidFromClientID(clientID), 10)
}

// lsOps is the part of messagix that sending, read receipts and media refresh use.
type lsOps interface {
	WaitReady(ctx context.Context) error
	ExecuteTasks(ctx context.Context, tasks ...socket.Task) (*table.LSTable, error)
	UploadMedia(ctx context.Context, threadID int64, filename, mime string, data []byte) (int64, error)
	Cursor(syncGroup int64) string
}

// waOps is the part of whatsmeow that sending, read receipts and media download use.
type waOps interface {
	WaitReady(ctx context.Context) error
	Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	// SendFBMessage fills meta (franking key) as whatsmeow does; the history upload needs it.
	SendFBMessage(ctx context.Context, to waTypes.JID, msg *waConsumerApplication.ConsumerApplication, meta *waMsgApplication.MessageApplication_Metadata, id string) (whatsmeow.SendResponse, error)
	MarkRead(ctx context.Context, ids []string, ts time.Time, chat, sender waTypes.JID) error
	DownloadToFile(ctx context.Context, ref mediaRef, mediaType whatsmeow.MediaType, file whatsmeow.File) error
}

type realLS struct{ c *messagix.Client }

func (r realLS) WaitReady(ctx context.Context) error {
	return r.c.WaitUntilCanSendMessages(ctx, sendReadyTimeout)
}
func (r realLS) ExecuteTasks(ctx context.Context, tasks ...socket.Task) (*table.LSTable, error) {
	return r.c.ExecuteTasks(ctx, tasks...)
}
func (r realLS) Cursor(syncGroup int64) string { return r.c.GetCursor(syncGroup) }

type realWA struct{ e *e2eeConn }

func (r realWA) cli() (*whatsmeow.Client, error) {
	if c := r.e.client(); c != nil {
		return c, nil
	}
	// Not ErrNotConnected: writeWA reads that as "the socket dropped after writing".
	return nil, whatsmeow.ErrClientIsNil
}

// WaitReady waits for the E2EE socket, which may be reconnecting (handlematrix.go:87 waits too).
func (r realWA) WaitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sendReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c := r.e.client(); c != nil && c.IsConnected() && c.IsLoggedIn() {
			return nil
		}
		select {
		case <-ctx.Done():
			return whatsmeow.ErrNotConnected
		case <-ticker.C:
		}
	}
}
func (r realWA) Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	c, err := r.cli()
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}
	return c.Upload(ctx, data, mediaType)
}
func (r realWA) SendFBMessage(ctx context.Context, to waTypes.JID, msg *waConsumerApplication.ConsumerApplication, meta *waMsgApplication.MessageApplication_Metadata, id string) (whatsmeow.SendResponse, error) {
	c, err := r.cli()
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}
	return c.SendFBMessage(ctx, to, msg, meta, whatsmeow.SendRequestExtra{ID: id})
}
func (r realWA) MarkRead(ctx context.Context, ids []string, ts time.Time, chat, sender waTypes.JID) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.MarkRead(ctx, ids, ts, chat, sender)
}
func (r realWA) DownloadToFile(ctx context.Context, ref mediaRef, mediaType whatsmeow.MediaType, file whatsmeow.File) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.DownloadMediaWithPathToFile(ctx, ref.DirectPath, ref.EncSHA256, ref.SHA256, ref.MediaKey, mediaType, "", false, file)
}

// liveSession is what one command needs from the current session.
type liveSession struct {
	index *ThreadIndex
	ctx   context.Context
	ls    lsOps // nil when there is no messagix client
	wa    waOps // nil when E2EE is not set up for this session
}

func (b *MetaBackend) liveSession() (liveSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.auth {
		return liveSession{}, &Error{Code: "auth_required", Message: "login required"}
	}
	if b.index == nil || b.client == nil {
		return liveSession{}, &Error{Code: "disconnected", Message: "no active session"}
	}
	s := liveSession{index: b.index, ctx: b.ctx, ls: b.lsOverride, wa: b.waOverride}
	if s.ctx == nil {
		s.ctx = context.Background()
	}
	if rc, ok := b.client.(*realMetaClient); ok && s.ls == nil {
		s.ls = realLS{rc.Client}
	}
	if s.wa == nil && b.e2ee != nil {
		s.wa = realWA{b.e2ee}
	}
	return s, nil
}

func (b *MetaBackend) confirmTimeout() time.Duration {
	if b.sendTimeout > 0 {
		return b.sendTimeout
	}
	return sendConfirmTimeout
}

// sendTarget is the thread a send goes to: a Lightspeed thread key or an E2EE chat.
type sendTarget struct {
	threadKey int64
	chat      waTypes.JID
	e2ee      bool
}

// prepareSend checks everything that can fail before a message is queued.
func (b *MetaBackend) prepareSend(convID, clientID string) (liveSession, sendTarget, error) {
	if clientID == "" || convID == "" {
		return liveSession{}, sendTarget{}, &Error{Code: "bad_request", Message: "conversation_id and client_id are required"}
	}
	s, err := b.liveSession()
	if err != nil {
		return s, sendTarget{}, err
	}
	thread, e2ee := s.index.SendRoute(convID)
	if thread == "" {
		return s, sendTarget{}, &Error{Code: "unknown_conversation", Message: "unknown conversation"}
	}
	t := sendTarget{e2ee: e2ee}
	if e2ee {
		if t.chat, err = waTypes.ParseJID(thread); err != nil {
			return s, t, &Error{Code: "internal", Message: "invalid E2EE thread"}
		}
		if s.wa == nil {
			return s, t, &Error{Code: "e2ee_unavailable", Message: "encrypted chats are not available"}
		}
	} else {
		if t.threadKey, err = strconv.ParseInt(thread, 10, 64); err != nil {
			return s, t, &Error{Code: "internal", Message: "invalid thread"}
		}
		if s.ls == nil {
			return s, t, &Error{Code: "disconnected", Message: "no active session"}
		}
	}
	return s, t, nil
}

// claimSend reserves client_id for one send. A second send with a client_id that is still queued or
// in flight is refused instead of replacing the first one's confirmation wait.
func (b *MetaBackend) claimSend(clientID string) error {
	if !b.sends.claim(clientID) {
		return &Error{Code: "duplicate", Message: "a send with this client_id is in progress"}
	}
	return nil
}

// queueSend runs deliver after the earlier sends of the same conversation and after the command's
// response was emitted, so message_send_result never comes before response{ok}.
func (b *MetaBackend) queueSend(ctx context.Context, convID, clientID string, deliver func()) {
	responded := responseSent(ctx)
	b.sends.enqueue(convID, func() {
		defer b.sends.release(clientID)
		if responded != nil {
			<-responded
		}
		deliver()
	})
}

func (b *MetaBackend) SendText(ctx context.Context, args SendTextArgs, emit func(Event)) error {
	if args.Text == "" {
		return &Error{Code: "bad_request", Message: "text is empty"}
	}
	s, target, err := b.prepareSend(args.ConversationID, args.ClientID)
	if err != nil {
		return err
	}
	if err = b.claimSend(args.ClientID); err != nil {
		return err
	}
	if target.e2ee {
		msg := consumerMessage(&waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{
			MessageText: &waCommon.MessageText{Text: proto.String(args.Text)},
		}})
		b.queueSend(ctx, args.ConversationID, args.ClientID, func() {
			b.deliverWA(s, target.chat, args.ClientID, emit, func(context.Context) (*waConsumerApplication.ConsumerApplication, error) { return msg, nil })
		})
		return nil
	}
	b.queueSend(ctx, args.ConversationID, args.ClientID, func() {
		b.deliverLS(s, args.ClientID, emit, func(_ context.Context, otid int64) (*socket.SendMessageTask, error) {
			task := newSendMessageTask(target.threadKey, otid)
			task.Text = args.Text
			return task, nil
		})
	})
	return nil
}

func (b *MetaBackend) SendMedia(ctx context.Context, args SendMediaArgs, emit func(Event)) error {
	s, target, err := b.prepareSend(args.ConversationID, args.ClientID)
	if err != nil {
		return err
	}
	if err = b.claimSend(args.ClientID); err != nil {
		return err
	}
	// Read the whole file now: size and readability are reported in the response, before any upload,
	// and later changes to the file do not affect what is sent.
	data, name, err := readUpload(args.FilePath)
	if err != nil {
		b.sends.release(args.ClientID)
		return err
	}
	mime := uploadMIME(args.MIME, data)
	if target.e2ee {
		b.queueSend(ctx, args.ConversationID, args.ClientID, func() {
			b.deliverWA(s, target.chat, args.ClientID, emit, func(ctx context.Context) (*waConsumerApplication.ConsumerApplication, error) {
				return waMediaMessage(ctx, s.wa, data, mime, name)
			})
		})
		return nil
	}
	b.queueSend(ctx, args.ConversationID, args.ClientID, func() {
		b.deliverLS(s, args.ClientID, emit, func(ctx context.Context, otid int64) (*socket.SendMessageTask, error) {
			// to-meta.go: Mercury upload, then the fbid goes into attachment_fbids.
			fbid, err := s.ls.UploadMedia(ctx, target.threadKey, name, mime, data)
			if err != nil {
				return nil, err
			}
			task := newSendMessageTask(target.threadKey, otid)
			task.SendType = table.MEDIA
			task.AttachmentFBIds = []int64{fbid}
			return task, nil
		})
	})
	return nil
}

// newSendMessageTask mirrors msgconv.ToMeta for a 1-1 thread.
func newSendMessageTask(threadKey, otid int64) *socket.SendMessageTask {
	return &socket.SendMessageTask{
		ThreadId:         threadKey,
		Otid:             otid,
		Source:           table.MESSENGER_INBOX_IN_THREAD,
		InitiatingSource: table.FACEBOOK_INBOX,
		SendType:         table.TEXT,
		SyncGroup:        1,
	}
}

func consumerMessage(content *waConsumerApplication.ConsumerApplication_Content) *waConsumerApplication.ConsumerApplication {
	return &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_Content{Content: content},
	}}
}

type sendOutcome struct {
	status    string // sent | failed | unknown
	messageID string
	code      string
}

var (
	outcomeNotConfirmed = sendOutcome{status: "unknown", code: "not_confirmed"}
	outcomeInternal     = sendOutcome{status: "failed", code: "internal"}
)

func emitSendResult(emit func(Event), clientID string, o sendOutcome) {
	if emit == nil {
		return
	}
	r := MessageSendResult{ClientID: clientID, Status: o.status}
	if o.status == "sent" {
		id := o.messageID
		r.MessageID = &id
	} else if o.code != "" {
		r.Error = &Error{Code: o.code, Message: sendErrorMessages[o.code]}
	}
	emit(Event{Type: EventMessageSendResult, Data: r})
}

var sendErrorMessages = map[string]string{
	"upload_failed": "uploading the file failed",
	"disconnected":  "not connected",
	"rejected":      "the server rejected the message",
	"not_confirmed": "the server did not confirm the message",
	"internal":      "internal error",
}

// sendReport emits the result of one send exactly once. A panic in the send goroutine still yields a
// result: failed (internal) before the message was handed to the socket, unknown after.
type sendReport struct {
	emit     func(Event)
	clientID string
	written  atomic.Bool
	outcome  *sendOutcome // set once the outcome is known
	reported bool
}

func (r *sendReport) send(o sendOutcome) {
	r.reported = true
	emitSendResult(r.emit, r.clientID, o)
}

// recover is deferred directly by the deliver functions. The panic value is dropped: it may contain
// message content.
func (r *sendReport) recover() {
	if recover() == nil || r.reported {
		return
	}
	switch {
	case r.outcome != nil:
		r.send(*r.outcome)
	case r.written.Load():
		r.send(outcomeNotConfirmed)
	default:
		r.send(outcomeInternal)
	}
}

// deliverLS writes the task once and reports the outcome. It never sends again.
func (b *MetaBackend) deliverLS(s liveSession, clientID string, emit func(Event), build func(context.Context, int64) (*socket.SendMessageTask, error)) {
	rep := &sendReport{emit: emit, clientID: clientID}
	defer rep.recover()
	otid := otidFromClientID(clientID)
	otidStr := strconv.FormatInt(otid, 10)
	// Before writing, so an echo that beats the response still finds its client_id.
	s.index.RememberClientID(otidStr, "", clientID)
	rep.send(b.writeLS(s, otid, otidStr, build, &rep.written))
}

func (b *MetaBackend) writeLS(s liveSession, otid int64, otidStr string, build func(context.Context, int64) (*socket.SendMessageTask, error), written *atomic.Bool) sendOutcome {
	prepCtx, cancel := context.WithTimeout(s.ctx, uploadTimeout)
	task, err := build(prepCtx, otid)
	cancel()
	if err != nil {
		return sendOutcome{status: "failed", code: "upload_failed"}
	}
	readyCtx, cancel := context.WithTimeout(s.ctx, sendReadyTimeout)
	err = s.ls.WaitReady(readyCtx)
	cancel()
	if err != nil {
		return sendOutcome{status: "failed", code: "disconnected"}
	}
	confirmed, ok := b.waits.addSend(otidStr)
	if !ok {
		// claimSend keeps client_ids unique, so this is an OTID collision; never take over its wait.
		return outcomeInternal
	}
	defer b.waits.removeSend(otidStr, confirmed)
	ctx, cancel := context.WithTimeout(s.ctx, b.confirmTimeout())
	defer cancel()
	type response struct {
		tbl *table.LSTable
		err error
	}
	done := make(chan response, 1)
	written.Store(true)
	go func() {
		defer func() {
			if recover() != nil {
				done <- response{err: errSendPanic}
			}
		}()
		tbl, err := s.ls.ExecuteTasks(ctx, task)
		done <- response{tbl, err}
	}()
	for {
		select {
		case o := <-confirmed:
			return o
		case r := <-done:
			done = nil
			if r.err != nil {
				if errors.Is(r.err, dgw.ErrSocketNotOpen) {
					return sendOutcome{status: "failed", code: "disconnected"} // nothing was written
				}
				// Written but the stream broke or timed out: it may have been delivered.
				return confirmedOr(confirmed)
			}
			if o, ok := sendOutcomeFrom(r.tbl, otidStr, true); ok {
				return o
			}
			// No verdict in the response; a confirmation can still arrive over the socket.
		case <-ctx.Done():
			return confirmedOr(confirmed)
		}
	}
}

var errSendPanic = errors.New("send panicked")

// confirmedOr is the outcome when the send stream broke or timed out: a confirmation that already
// arrived over the socket wins over "not confirmed".
func confirmedOr(confirmed <-chan sendOutcome) sendOutcome {
	select {
	case o := <-confirmed:
		return o
	default:
		return outcomeNotConfirmed
	}
}

// sendOutcomeFrom finds the verdict on otid in a Lightspeed table (handlematrix.go:180-199). In the
// direct response of the send, a failed task without an OTID is ours too.
func sendOutcomeFrom(tbl *table.LSTable, otid string, response bool) (sendOutcome, bool) {
	if tbl == nil {
		return sendOutcome{}, false
	}
	for _, r := range tbl.LSReplaceOptimsiticMessage {
		if r.OfflineThreadingId == otid && r.MessageId != "" {
			return sendOutcome{status: "sent", messageID: r.MessageId}, true
		}
	}
	for _, f := range tbl.LSMarkOptimisticMessageFailed {
		if f.OTID == otid {
			return sendOutcome{status: "failed", code: "rejected"}, true
		}
	}
	for _, f := range tbl.LSHandleFailedTask {
		if f.OTID == otid || (response && f.OTID == "") {
			return sendOutcome{status: "failed", code: "rejected"}, true
		}
	}
	return sendOutcome{}, false
}

// E2EESentHook is called after an E2EE message was sent successfully, with the message and the
// application metadata (franking key) as sent. It runs on the conversation's send queue after
// message_send_result was emitted, so a slow hook must start its own goroutine. NewMetaBackend sets it
// to queueHistoryUpload, which adds sent messages to the secure history backup.
type E2EESentHook func(ctx context.Context, chat waTypes.JID, messageID string, msg *waConsumerApplication.ConsumerApplication, meta *waMsgApplication.MessageApplication_Metadata, ts time.Time)

// deliverWA sends the E2EE message once with the ID derived from client_id and reports the outcome.
// The sending device gets no echo from the server, so a successful send is echoed locally through the
// index, carrying the client_id and the same ID as the send result.
func (b *MetaBackend) deliverWA(s liveSession, chat waTypes.JID, clientID string, emit func(Event), build func(context.Context) (*waConsumerApplication.ConsumerApplication, error)) {
	rep := &sendReport{emit: emit, clientID: clientID}
	defer rep.recover()
	id := waMessageID(clientID)
	s.index.RememberClientID("", id, clientID)
	o, msg, meta, ts := b.writeWA(s, chat, id, build, &rep.written)
	rep.outcome = &o
	if o.status == "sent" && emit != nil {
		evt := &events.FBMessage{Message: msg}
		evt.Info.ID = id
		evt.Info.Chat = chat
		evt.Info.Sender = waTypes.NewJID(strconv.FormatInt(s.index.self, 10), waTypes.MessengerServer)
		evt.Info.IsFromMe = true
		evt.Info.Timestamp = ts
		s.index.ApplyWAMessage(evt, emit)
	}
	rep.send(o)
	if o.status == "sent" && b.onE2EESent != nil {
		b.onE2EESent(s.ctx, chat, id, msg, meta, ts)
	}
}

func (b *MetaBackend) writeWA(s liveSession, chat waTypes.JID, id string, build func(context.Context) (*waConsumerApplication.ConsumerApplication, error), written *atomic.Bool) (sendOutcome, *waConsumerApplication.ConsumerApplication, *waMsgApplication.MessageApplication_Metadata, time.Time) {
	readyCtx, cancel := context.WithTimeout(s.ctx, sendReadyTimeout)
	err := s.wa.WaitReady(readyCtx)
	cancel()
	if err != nil {
		return sendOutcome{status: "failed", code: "disconnected"}, nil, nil, time.Time{}
	}
	prepCtx, cancel := context.WithTimeout(s.ctx, uploadTimeout)
	msg, err := build(prepCtx)
	cancel()
	if err != nil {
		return sendOutcome{status: "failed", code: "upload_failed"}, nil, nil, time.Time{}
	}
	ctx, cancel := context.WithTimeout(s.ctx, b.confirmTimeout())
	defer cancel()
	written.Store(true)
	meta := &waMsgApplication.MessageApplication_Metadata{}
	resp, err := s.wa.SendFBMessage(ctx, chat, msg, meta, id)
	switch {
	case err == nil:
		ts := resp.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		return sendOutcome{status: "sent", messageID: id}, msg, meta, ts
	case errors.Is(err, whatsmeow.ErrServerReturnedError):
		return sendOutcome{status: "failed", code: "rejected"}, nil, nil, time.Time{}
	case errors.Is(err, whatsmeow.ErrNotLoggedIn), errors.Is(err, whatsmeow.ErrClientIsNil):
		return sendOutcome{status: "failed", code: "disconnected"}, nil, nil, time.Time{} // refused before writing
	case errors.Is(err, whatsmeow.ErrRecipientADJID), errors.Is(err, whatsmeow.ErrUnknownServer):
		return outcomeInternal, nil, nil, time.Time{} // invalid chat JID, refused before writing
	default:
		// Timed out or the socket dropped after the message may have been written. This includes
		// ErrNotConnected: request.go retryFrame returns it after the node was already sent.
		return outcomeNotConfirmed, nil, nil, time.Time{}
	}
}

// MarkRead marks a conversation read up to until_ts in every thread it has: the normal Lightspeed
// thread, the Lightspeed row of the E2EE thread (sync group 95) and the E2EE chat itself, where read
// receipts go to the peer's recent incoming messages (handlematrix.go:613-640).
func (b *MetaBackend) MarkRead(_ context.Context, args MarkReadArgs, _ func(Event)) error {
	s, err := b.liveSession()
	if err != nil {
		return err
	}
	until := args.UntilTS
	if until <= 0 {
		until = time.Now().UnixMilli()
	}
	t, ok := s.index.takeReadTargets(args.ConversationID, until)
	if !ok {
		return &Error{Code: "unknown_conversation", Message: "unknown conversation"}
	}
	ctx, cancel := context.WithTimeout(s.ctx, markReadTimeout)
	defer cancel()
	var tasks []socket.Task
	if t.normalThread != 0 {
		tasks = append(tasks, &socket.ThreadMarkReadTask{ThreadId: t.normalThread, LastReadWatermarkTs: until, SyncGroup: 1})
	}
	if t.e2eeLSKey != 0 {
		// E2EE timestamps have second precision; a whole second is read up to its last millisecond
		// (handlematrix.go:585-590).
		watermark := until
		if watermark%1000 == 0 {
			watermark += 999
		}
		tasks = append(tasks, &socket.ThreadMarkReadTask{ThreadId: t.e2eeLSKey, LastReadWatermarkTs: watermark, SyncGroup: e2eeSyncGroup})
	}
	var failed bool
	if len(tasks) > 0 {
		if s.ls == nil {
			failed = true
		} else if _, err = s.ls.ExecuteTasks(ctx, tasks...); err != nil {
			failed = true
		}
	}
	if len(t.waIDs) > 0 && !t.e2eeChat.IsEmpty() {
		if s.wa == nil || s.wa.MarkRead(ctx, t.waIDs, time.Now(), t.e2eeChat, t.e2eeChat) != nil {
			// The receipts were not sent: keep the IDs for the next mark_read.
			s.index.restoreUnread(args.ConversationID, t.waTaken)
			failed = true
		}
	}
	if failed {
		return &Error{Code: "disconnected", Message: "failed to mark read"}
	}
	return nil
}

// confirmWaits hands send confirmations and refreshed media URLs that arrive over the socket to the
// command waiting for them. The zero value is ready to use.
type confirmWaits struct {
	mu    sync.Mutex
	sends map[string]chan sendOutcome
	media map[string][]chan string
}

// addSend registers the wait for otid. It refuses (false) while another send waits on the same OTID.
func (w *confirmWaits) addSend(otid string) (chan sendOutcome, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sends == nil {
		w.sends = map[string]chan sendOutcome{}
	}
	if _, busy := w.sends[otid]; busy {
		return nil, false
	}
	ch := make(chan sendOutcome, 1)
	w.sends[otid] = ch
	return ch, true
}

func (w *confirmWaits) removeSend(otid string, ch chan sendOutcome) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sends[otid] == ch {
		delete(w.sends, otid)
	}
}

func (w *confirmWaits) addMedia(fbid string) chan string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.media == nil {
		w.media = map[string][]chan string{}
	}
	ch := make(chan string, 1)
	w.media[fbid] = append(w.media[fbid], ch)
	return ch
}

func (w *confirmWaits) removeMedia(fbid string, ch chan string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.media[fbid]
	for i, c := range list {
		if c == ch {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(w.media, fbid)
	} else {
		w.media[fbid] = list
	}
}

func (w *confirmWaits) observe(tbl *table.LSTable) {
	if tbl == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for otid, ch := range w.sends {
		if o, ok := sendOutcomeFrom(tbl, otid, false); ok {
			select {
			case ch <- o:
			default:
			}
		}
	}
	if len(w.media) == 0 {
		return
	}
	for fbid, url := range tableMediaURLs(tbl) {
		for _, ch := range w.media[fbid] {
			select {
			case ch <- url:
			default:
			}
		}
	}
}

// sendQueue runs the sends of one conversation one after another, in command order, so an upload of a
// later message never delays an earlier one; other conversations run in parallel. It also tracks the
// client_ids that are queued or in flight. The zero value is ready to use.
type sendQueue struct {
	mu      sync.Mutex
	convs   map[string][]func() // present while a worker runs for the conversation
	pending map[string]bool
}

func (q *sendQueue) claim(clientID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = map[string]bool{}
	}
	if q.pending[clientID] {
		return false
	}
	q.pending[clientID] = true
	return true
}

func (q *sendQueue) release(clientID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, clientID)
}

func (q *sendQueue) enqueue(convID string, job func()) {
	q.mu.Lock()
	if q.convs == nil {
		q.convs = map[string][]func(){}
	}
	jobs, running := q.convs[convID]
	q.convs[convID] = append(jobs, job)
	q.mu.Unlock()
	if !running {
		go q.run(convID)
	}
}

func (q *sendQueue) run(convID string) {
	for {
		q.mu.Lock()
		jobs := q.convs[convID]
		if len(jobs) == 0 {
			delete(q.convs, convID)
			q.mu.Unlock()
			return
		}
		job := jobs[0]
		jobs[0] = nil
		q.convs[convID] = jobs[1:]
		q.mu.Unlock()
		runJob(job)
	}
}

// runJob keeps the worker alive when a job panics; deliverLS/deliverWA report their own panics.
func runJob(job func()) {
	defer func() { _ = recover() }()
	job()
}
