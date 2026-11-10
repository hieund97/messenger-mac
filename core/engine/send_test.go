package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
)

// syncLog collects events emitted from send goroutines.
type syncLog struct {
	mu     sync.Mutex
	events []Event
	added  chan struct{}
}

func newSyncLog() *syncLog { return &syncLog{added: make(chan struct{}, 1000)} }

func (l *syncLog) emit(e Event) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
	select {
	case l.added <- struct{}{}:
	default:
	}
}

func (l *syncLog) ofType(t string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// waitFor waits until at least n events of type t were emitted.
func (l *syncLog) waitFor(t *testing.T, typ string, n int) []Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if got := l.ofType(typ); len(got) >= n {
			return got
		}
		select {
		case <-l.added:
		case <-deadline:
			t.Fatalf("timed out waiting for %d %s events, have %d", n, typ, len(l.ofType(typ)))
		}
	}
}

type fakeLS struct {
	mu       sync.Mutex
	tasks    [][]socket.Task
	uploads  int
	execute  func(ctx context.Context, tasks []socket.Task) (*table.LSTable, error)
	upload   func(ctx context.Context) (int64, error) // nil returns uploadID
	uploadID int64
}

func (f *fakeLS) WaitReady(context.Context) error { return nil }
func (f *fakeLS) Cursor(int64) string             { return "" }
func (f *fakeLS) ExecuteTasks(ctx context.Context, tasks ...socket.Task) (*table.LSTable, error) {
	f.mu.Lock()
	f.tasks = append(f.tasks, tasks)
	exec := f.execute
	f.mu.Unlock()
	if exec == nil {
		return &table.LSTable{}, nil
	}
	return exec(ctx, tasks)
}
func (f *fakeLS) UploadMedia(ctx context.Context, _ int64, _, _ string, _ []byte) (int64, error) {
	f.mu.Lock()
	f.uploads++
	upload, id := f.upload, f.uploadID
	f.mu.Unlock()
	if upload != nil {
		return upload(ctx)
	}
	return id, nil
}
func (f *fakeLS) calls() [][]socket.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]socket.Task(nil), f.tasks...)
}

type waReadCall struct {
	ids        []string
	chat, from waTypes.JID
}

type fakeWA struct {
	mu      sync.Mutex
	sends   []string
	uploads int
	reads   []waReadCall
	sent    []*waConsumerApplication.ConsumerApplication
	send    func(ctx context.Context, id string) (whatsmeow.SendResponse, error)
	markErr error
}

func (f *fakeWA) WaitReady(context.Context) error { return nil }
func (f *fakeWA) Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads++
	return whatsmeow.UploadResponse{DirectPath: "/v/t62/x", MediaKey: make([]byte, 32), FileSHA256: make([]byte, 32), FileEncSHA256: make([]byte, 32)}, nil
}
func (f *fakeWA) SendFBMessage(ctx context.Context, _ waTypes.JID, msg *waConsumerApplication.ConsumerApplication, meta *waMsgApplication.MessageApplication_Metadata, id string) (whatsmeow.SendResponse, error) {
	if meta != nil {
		meta.FrankingKey = make([]byte, 32) // whatsmeow sets a random franking key
	}
	f.mu.Lock()
	f.sends = append(f.sends, id)
	f.sent = append(f.sent, msg)
	send := f.send
	f.mu.Unlock()
	if send == nil {
		return whatsmeow.SendResponse{ID: id, Timestamp: time.UnixMilli(7000)}, nil
	}
	return send(ctx, id)
}
func (f *fakeWA) MarkRead(_ context.Context, ids []string, _ time.Time, chat, sender waTypes.JID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.reads = append(f.reads, waReadCall{ids: ids, chat: chat, from: sender})
	return nil
}
func (f *fakeWA) DownloadToFile(context.Context, mediaRef, whatsmeow.MediaType, whatsmeow.File) error {
	return errors.New("not used")
}

const testE2EEPeer int64 = 200

func newSendBackend(t *testing.T) (*MetaBackend, *syncLog, *fakeLS, *fakeWA) {
	t.Helper()
	b := NewMetaBackend(Config{})
	log := newSyncLog()
	b.index = NewThreadIndex(testSelf)
	b.client = &testMetaClient{}
	b.emit = log.emit
	ls := &fakeLS{uploadID: 4242}
	wa := &fakeWA{}
	b.lsOverride, b.waOverride = ls, wa
	// u:<testPeer> is a normal thread, u:200 an E2EE-only chat.
	b.index.ApplyTable(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
		{ThreadKey: testPeer, ThreadType: table.ONE_TO_ONE, LastActivityTimestampMs: 1000},
	}}, func(Event) {})
	peer := waTypes.NewJID(strconv.FormatInt(testE2EEPeer, 10), waTypes.MessengerServer)
	b.index.ApplyWAMessage(textWA("wa-in-1", peer, peer, "a"), func(Event) {})
	return b, log, ls, wa
}

func sendResults(log *syncLog) []MessageSendResult {
	var out []MessageSendResult
	for _, e := range log.ofType(EventMessageSendResult) {
		out = append(out, e.Data.(MessageSendResult))
	}
	return out
}

func TestOTIDStableForSameClientID(t *testing.T) {
	a := otidFromClientID("6F1C8E2A-0000-4000-8000-000000000001")
	if a <= 0 {
		t.Fatalf("otid must be positive, got %d", a)
	}
	for i := 0; i < 3; i++ {
		if again := otidFromClientID("6F1C8E2A-0000-4000-8000-000000000001"); again != a {
			t.Fatalf("otid changed: %d != %d", again, a)
		}
	}
	if b := otidFromClientID("6F1C8E2A-0000-4000-8000-000000000002"); b == a || b <= 0 {
		t.Fatalf("different client_id gave %d (first %d)", b, a)
	}
	// Shaped like methods.GenerateEpochID: milliseconds in the upper bits.
	if ms := a >> 22; ms < time.Now().Add(-time.Hour).UnixMilli() || ms > time.Now().Add(time.Hour).UnixMilli() {
		t.Fatalf("otid timestamp %d out of range", ms)
	}
	if waMessageID("x") != strconv.FormatInt(otidFromClientID("x"), 10) {
		t.Fatal("E2EE message ID must be the decimal OTID")
	}
}

func TestSendTimeoutYieldsUnknownNotRetry(t *testing.T) {
	b, log, ls, wa := newSendBackend(t)
	b.sendTimeout = 50 * time.Millisecond
	ls.execute = func(ctx context.Context, _ []socket.Task) (*table.LSTable, error) {
		<-ctx.Done() // written, never confirmed
		return nil, ctx.Err()
	}
	wa.send = func(ctx context.Context, _ string) (whatsmeow.SendResponse, error) {
		<-ctx.Done()
		return whatsmeow.SendResponse{}, ctx.Err()
	}
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: conversationID(testSelf, testPeer), Text: "hi", ClientID: "c-ls"}, log.emit); err != nil {
		t.Fatal(err)
	}
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "hi", ClientID: "c-wa"}, log.emit); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, EventMessageSendResult, 2)
	time.Sleep(200 * time.Millisecond) // a retry, if any, would show up here
	for _, r := range sendResults(log) {
		if r.Status != "unknown" || r.MessageID != nil {
			t.Fatalf("result = %+v", r)
		}
	}
	if n := len(ls.calls()); n != 1 {
		t.Fatalf("LS send called %d times", n)
	}
	wa.mu.Lock()
	defer wa.mu.Unlock()
	if len(wa.sends) != 1 {
		t.Fatalf("E2EE send called %d times", len(wa.sends))
	}
}

func TestSendResultFromLightspeedResponse(t *testing.T) {
	b, log, ls, _ := newSendBackend(t)
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		otid := strconv.FormatInt(tasks[0].(*socket.SendMessageTask).Otid, 10)
		if tasks[0].(*socket.SendMessageTask).Text == "bad" {
			return &table.LSTable{LSMarkOptimisticMessageFailed: []*table.LSMarkOptimisticMessageFailed{{OTID: otid}}}, nil
		}
		return &table.LSTable{LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: otid, MessageId: "mid.$ok"}}}, nil
	}
	conv := conversationID(testSelf, testPeer)
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: conv, Text: "ok", ClientID: "c-ok"}, log.emit)
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: conv, Text: "bad", ClientID: "c-bad"}, log.emit)
	log.waitFor(t, EventMessageSendResult, 2)
	got := map[string]MessageSendResult{}
	for _, r := range sendResults(log) {
		got[r.ClientID] = r
	}
	if r := got["c-ok"]; r.Status != "sent" || r.MessageID == nil || *r.MessageID != "mid.$ok" {
		t.Fatalf("ok = %+v", r)
	}
	if r := got["c-bad"]; r.Status != "failed" || r.MessageID != nil {
		t.Fatalf("bad = %+v", r)
	}
}

func TestEchoCarriesClientIDAndMatchesSendResultID(t *testing.T) {
	b, log, ls, wa := newSendBackend(t)
	var otid string
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		otid = strconv.FormatInt(tasks[0].(*socket.SendMessageTask).Otid, 10)
		return &table.LSTable{LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: otid, MessageId: "mid.$echo"}}}, nil
	}
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: conversationID(testSelf, testPeer), Text: "hi", ClientID: "c-ls"}, log.emit); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, EventMessageSendResult, 1)
	// The Lightspeed echo arrives over the socket with the OTID that was sent.
	b.handleTable(b.client, &table.LSTable{LSInsertMessage: []*table.LSInsertMessage{
		{ThreadKey: testPeer, MessageId: "mid.$echo", OfflineThreadingId: otid, SenderId: testSelf, TimestampMs: 6000, Text: "hi"},
	}})
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "yo", ClientID: "c-wa"}, log.emit); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, EventMessageSendResult, 2)

	results := map[string]MessageSendResult{}
	for _, r := range sendResults(log) {
		results[r.ClientID] = r
	}
	echoes := map[string]Message{}
	for _, e := range log.ofType(EventMessageUpsert) {
		m := e.Data.(Message)
		if m.ClientID != nil {
			echoes[*m.ClientID] = m
		}
	}
	for _, id := range []string{"c-ls", "c-wa"} {
		r, m := results[id], echoes[id]
		if r.Status != "sent" || r.MessageID == nil {
			t.Fatalf("%s result = %+v", id, r)
		}
		if m.ID == "" || m.ID != *r.MessageID || !m.IsOutgoing {
			t.Fatalf("%s echo = %+v, result id %s", id, m, *r.MessageID)
		}
	}
	if *results["c-wa"].MessageID != waMessageID("c-wa") || echoes["c-wa"].ConversationID != "u:200" || !echoes["c-wa"].E2EE {
		t.Fatalf("E2EE echo = %+v", echoes["c-wa"])
	}
	wa.mu.Lock()
	defer wa.mu.Unlock()
	if len(wa.sends) != 1 || wa.sends[0] != waMessageID("c-wa") {
		t.Fatalf("E2EE sends = %v", wa.sends)
	}
}

func TestSendUnknownConversationRejected(t *testing.T) {
	b, log, ls, _ := newSendBackend(t)
	err := b.SendText(context.Background(), SendTextArgs{ConversationID: "u:404", Text: "x", ClientID: "c"}, log.emit)
	var e *Error
	if !errors.As(err, &e) || e.Code != "unknown_conversation" {
		t.Fatalf("err = %v", err)
	}
	if len(ls.calls()) != 0 || len(log.ofType(EventMessageSendResult)) != 0 {
		t.Fatal("nothing must be sent")
	}
}

// confirmAll answers every SendMessageTask with a server message ID.
func confirmAll(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
	tbl := &table.LSTable{}
	for _, task := range tasks {
		if send, ok := task.(*socket.SendMessageTask); ok {
			otid := strconv.FormatInt(send.Otid, 10)
			tbl.LSReplaceOptimsiticMessage = append(tbl.LSReplaceOptimsiticMessage, &table.LSReplaceOptimsiticMessage{OfflineThreadingId: otid, MessageId: "mid.$" + otid})
		}
	}
	return tbl, nil
}

func writeSized(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return path
}

func TestFileOver25MBRejectedBeforeUpload(t *testing.T) {
	b, log, ls, wa := newSendBackend(t)
	path := writeSized(t, maxUploadBytes+1)
	for _, conv := range []string{conversationID(testSelf, testPeer), "u:200"} {
		err := b.SendMedia(context.Background(), SendMediaArgs{ConversationID: conv, FilePath: path, MIME: "application/pdf", ClientID: "c-big"}, log.emit)
		var e *Error
		if !errors.As(err, &e) || e.Code != "file_too_large" {
			t.Fatalf("%s: err = %v", conv, err)
		}
	}
	if ls.uploads != 0 || wa.uploads != 0 || len(ls.calls()) != 0 {
		t.Fatal("upload must not be called")
	}
	// Exactly 25 MB is allowed.
	ls.execute = confirmAll
	ok := writeSized(t, maxUploadBytes)
	if err := b.SendMedia(context.Background(), SendMediaArgs{ConversationID: conversationID(testSelf, testPeer), FilePath: ok, MIME: "application/pdf", ClientID: "c-ok"}, log.emit); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, EventMessageSendResult, 1)
	calls := ls.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if task := calls[0][0].(*socket.SendMessageTask); len(task.AttachmentFBIds) != 1 || task.AttachmentFBIds[0] != 4242 || task.SendType != table.MEDIA {
		t.Fatalf("task = %+v", task)
	}
}

func TestUnreadableFileRejected(t *testing.T) {
	b, log, ls, wa := newSendBackend(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.pdf")
	if err := os.WriteFile(locked, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing.pdf"), dir, locked} {
		if path == locked && os.Geteuid() == 0 {
			continue // root reads mode-0 files
		}
		err := b.SendMedia(context.Background(), SendMediaArgs{ConversationID: "u:200", FilePath: path, MIME: "application/pdf", ClientID: "c"}, log.emit)
		var e *Error
		if !errors.As(err, &e) || e.Code != "file_unreadable" {
			t.Fatalf("%s: err = %v", filepath.Base(path), err)
		}
	}
	if ls.uploads != 0 || wa.uploads != 0 {
		t.Fatal("upload must not be called")
	}
}

func TestMarkReadRoutesByThreadType(t *testing.T) {
	b, log, ls, wa := newSendBackend(t)
	peer := waTypes.NewJID("200", waTypes.MessengerServer)
	late := textWA("wa-in-late", peer, peer, "b")
	late.Info.Timestamp = time.UnixMilli(9000)
	b.index.ApplyWAMessage(late, func(Event) {}) // newer than until_ts, stays unread
	mine := textWA("wa-out", peer, waTypes.NewJID(strconv.FormatInt(testSelf, 10), waTypes.MessengerServer), "c")
	b.index.ApplyWAMessage(mine, func(Event) {}) // own message, never marked

	if err := b.MarkRead(context.Background(), MarkReadArgs{ConversationID: conversationID(testSelf, testPeer), UntilTS: 1500}, log.emit); err != nil {
		t.Fatal(err)
	}
	calls := ls.calls()
	if len(calls) != 1 || len(calls[0]) != 1 {
		t.Fatalf("LS calls = %+v", calls)
	}
	if task, ok := calls[0][0].(*socket.ThreadMarkReadTask); !ok || task.ThreadId != testPeer || task.LastReadWatermarkTs != 1500 || task.SyncGroup != 1 || task.GetLabel() != "21" {
		t.Fatalf("task = %+v", calls[0][0])
	}
	wa.mu.Lock()
	if len(wa.reads) != 0 {
		t.Fatalf("normal thread must not use E2EE: %+v", wa.reads)
	}
	wa.mu.Unlock()

	if err := b.MarkRead(context.Background(), MarkReadArgs{ConversationID: "u:200", UntilTS: 6000}, log.emit); err != nil {
		t.Fatal(err)
	}
	if len(ls.calls()) != 1 {
		t.Fatal("E2EE-only chat must not use Lightspeed")
	}
	wa.mu.Lock()
	defer wa.mu.Unlock()
	if len(wa.reads) != 1 || len(wa.reads[0].ids) != 1 || wa.reads[0].ids[0] != "wa-in-1" || wa.reads[0].chat != peer {
		t.Fatalf("E2EE reads = %+v", wa.reads)
	}
}

func resultsByClient(log *syncLog) map[string]MessageSendResult {
	out := map[string]MessageSendResult{}
	for _, r := range sendResults(log) {
		out[r.ClientID] = r
	}
	return out
}

func resultCode(r MessageSendResult) string {
	if r.Error == nil {
		return ""
	}
	return r.Error.Code
}

func TestE2EESendErrorsClassified(t *testing.T) {
	b, log, _, wa := newSendBackend(t)
	cases := map[string]struct {
		err          error
		status, code string
	}{
		"c-notconn":  {whatsmeow.ErrNotConnected, "unknown", "not_confirmed"}, // retryFrame: may be written
		"c-notlogin": {whatsmeow.ErrNotLoggedIn, "failed", "disconnected"},
		"c-nilcli":   {whatsmeow.ErrClientIsNil, "failed", "disconnected"},
		"c-adjid":    {whatsmeow.ErrRecipientADJID, "failed", "internal"},
		"c-server":   {whatsmeow.ErrUnknownServer, "failed", "internal"},
		"c-rejected": {whatsmeow.ErrServerReturnedError, "failed", "rejected"},
	}
	byID := map[string]error{}
	for clientID, c := range cases {
		byID[waMessageID(clientID)] = c.err
	}
	wa.send = func(_ context.Context, id string) (whatsmeow.SendResponse, error) {
		return whatsmeow.SendResponse{}, byID[id]
	}
	for clientID := range cases {
		if err := b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "x", ClientID: clientID}, log.emit); err != nil {
			t.Fatal(err)
		}
	}
	log.waitFor(t, EventMessageSendResult, len(cases))
	got := resultsByClient(log)
	for clientID, c := range cases {
		if r := got[clientID]; r.Status != c.status || resultCode(r) != c.code {
			t.Errorf("%s: result = %s/%s, want %s/%s", clientID, r.Status, resultCode(r), c.status, c.code)
		}
	}
}

func TestSendPanicStillReportsResult(t *testing.T) {
	b, log, ls, wa := newSendBackend(t)
	s, err := b.liveSession()
	if err != nil {
		t.Fatal(err)
	}
	peer := waTypes.NewJID("200", waTypes.MessengerServer)
	// Before anything is written: failed/internal.
	b.deliverLS(s, "c-ls-build", log.emit, func(context.Context, int64) (*socket.SendMessageTask, error) { panic("build") })
	b.deliverWA(s, peer, "c-wa-build", log.emit, func(context.Context) (*waConsumerApplication.ConsumerApplication, error) { panic("build") })
	// While writing: unknown.
	ls.execute = func(context.Context, []socket.Task) (*table.LSTable, error) { panic("write") }
	wa.send = func(context.Context, string) (whatsmeow.SendResponse, error) { panic("write") }
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: conversationID(testSelf, testPeer), Text: "x", ClientID: "c-ls-write"}, log.emit)
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "x", ClientID: "c-wa-write"}, log.emit)
	log.waitFor(t, EventMessageSendResult, 4)
	got := resultsByClient(log)
	for id, want := range map[string]string{"c-ls-build": "failed", "c-wa-build": "failed", "c-ls-write": "unknown", "c-wa-write": "unknown"} {
		if r := got[id]; r.Status != want {
			t.Errorf("%s: result = %+v, want %s", id, r, want)
		}
	}
	if r := got["c-ls-build"]; resultCode(r) != "internal" {
		t.Errorf("code = %s", resultCode(r))
	}
	// The conversation queues keep working.
	ls.execute = confirmAll
	wa.send = nil
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: conversationID(testSelf, testPeer), Text: "x", ClientID: "c-ls-after"}, log.emit)
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "x", ClientID: "c-wa-after"}, log.emit)
	log.waitFor(t, EventMessageSendResult, 6)
	if got = resultsByClient(log); got["c-ls-after"].Status != "sent" || got["c-wa-after"].Status != "sent" {
		t.Fatalf("after panic: %+v %+v", got["c-ls-after"], got["c-wa-after"])
	}
	if n := len(sendResults(log)); n != 6 {
		t.Fatalf("results = %d, want exactly one per send", n)
	}
}

func TestMarkReadKeepsE2EEIDsWhenReceiptFails(t *testing.T) {
	b, log, _, wa := newSendBackend(t)
	wa.markErr = errors.New("socket closed")
	if err := b.MarkRead(context.Background(), MarkReadArgs{ConversationID: "u:200", UntilTS: 6000}, log.emit); err == nil {
		t.Fatal("expected an error")
	}
	wa.mu.Lock()
	wa.markErr = nil
	wa.mu.Unlock()
	if err := b.MarkRead(context.Background(), MarkReadArgs{ConversationID: "u:200", UntilTS: 6000}, log.emit); err != nil {
		t.Fatal(err)
	}
	wa.mu.Lock()
	defer wa.mu.Unlock()
	if len(wa.reads) != 1 || len(wa.reads[0].ids) != 1 || wa.reads[0].ids[0] != "wa-in-1" {
		t.Fatalf("reads = %+v", wa.reads)
	}
}

func TestRestoreUnreadKeepsCapacity(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	peer := waTypes.NewJID("200", waTypes.MessengerServer)
	for i := 0; i < waUnreadCapacity; i++ {
		evt := textWA("new-"+strconv.Itoa(i), peer, peer, "x")
		evt.Info.Timestamp = time.UnixMilli(int64(10000 + i))
		idx.ApplyWAMessage(evt, func(Event) {})
	}
	idx.restoreUnread("u:200", []waIncoming{{id: "old", ts: 1}})
	list := idx.waUnread["u:200"]
	if len(list) != waUnreadCapacity || list[0].id != "new-0" {
		t.Fatalf("len %d, first %s", len(list), list[0].id)
	}
}

func TestMarkReadE2EELightspeedWatermarkCoversSecond(t *testing.T) {
	b, log, ls, _ := newSendBackend(t)
	b.index.ApplyTable(&table.LSTable{
		LSUpdateThreadAuthorityAndMappingWithOTIDFromJID: []*table.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID{{ThreadKey: hybridKey, ThreadJID: testE2EEPeer}},
		LSDeleteThenInsertThread:                         []*table.LSDeleteThenInsertThread{{ThreadKey: hybridKey, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE}},
	}, func(Event) {})
	for until, want := range map[int64]int64{6000: 6999, 6001: 6001} {
		before := len(ls.calls())
		if err := b.MarkRead(context.Background(), MarkReadArgs{ConversationID: "u:200", UntilTS: until}, log.emit); err != nil {
			t.Fatal(err)
		}
		calls := ls.calls()
		if len(calls) != before+1 {
			t.Fatalf("LS calls = %d", len(calls))
		}
		task, ok := calls[len(calls)-1][0].(*socket.ThreadMarkReadTask)
		if !ok || task.ThreadId != hybridKey || task.SyncGroup != e2eeSyncGroup || task.LastReadWatermarkTs != want {
			t.Fatalf("until %d: task = %+v", until, calls[len(calls)-1][0])
		}
	}
}

func TestSendsOfOneConversationKeepOrder(t *testing.T) {
	b, log, ls, wa := newSendBackend(t)
	release := make(chan struct{})
	uploading := make(chan struct{})
	ls.upload = func(ctx context.Context) (int64, error) {
		close(uploading)
		<-release
		return 4242, nil
	}
	var order []string
	var mu sync.Mutex
	ls.execute = func(ctx context.Context, tasks []socket.Task) (*table.LSTable, error) {
		mu.Lock()
		order = append(order, strconv.Itoa(int(tasks[0].(*socket.SendMessageTask).SendType)))
		mu.Unlock()
		return confirmAll(ctx, tasks)
	}
	path := filepath.Join(t.TempDir(), "a.pdf")
	if err := os.WriteFile(path, []byte("%PDF"), 0600); err != nil {
		t.Fatal(err)
	}
	conv := conversationID(testSelf, testPeer)
	if err := b.SendMedia(context.Background(), SendMediaArgs{ConversationID: conv, FilePath: path, MIME: "application/pdf", ClientID: "c-1"}, log.emit); err != nil {
		t.Fatal(err)
	}
	<-uploading
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: conv, Text: "second", ClientID: "c-2"}, log.emit); err != nil {
		t.Fatal(err)
	}
	// Another conversation is not held up by the upload.
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "other", ClientID: "c-other"}, log.emit); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, EventMessageSendResult, 1)
	time.Sleep(50 * time.Millisecond)
	if got := sendResults(log); len(got) != 1 || got[0].ClientID != "c-other" || len(ls.calls()) != 0 {
		t.Fatalf("before upload finished: results %+v, LS calls %d", got, len(ls.calls()))
	}
	close(release)
	log.waitFor(t, EventMessageSendResult, 3)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != strconv.Itoa(int(table.MEDIA)) || order[1] != strconv.Itoa(int(table.TEXT)) {
		t.Fatalf("write order = %v", order)
	}
	results := sendResults(log)
	if results[1].ClientID != "c-1" || results[2].ClientID != "c-2" {
		t.Fatalf("result order = %+v", results)
	}
	wa.mu.Lock()
	defer wa.mu.Unlock()
	if len(wa.sends) != 1 {
		t.Fatalf("E2EE sends = %d", len(wa.sends))
	}
}

func TestSendResultWaitsForResponse(t *testing.T) {
	b, log, ls, _ := newSendBackend(t)
	ls.execute = confirmAll
	responded := make(chan struct{})
	ctx := context.WithValue(context.Background(), responseSentKey{}, responded)
	if err := b.SendText(ctx, SendTextArgs{ConversationID: conversationID(testSelf, testPeer), Text: "x", ClientID: "c-1"}, log.emit); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(log.ofType(EventMessageSendResult)); n != 0 {
		t.Fatalf("result emitted before the response: %d", n)
	}
	close(responded)
	log.waitFor(t, EventMessageSendResult, 1)
}

func TestEngineEmitsSendResponseBeforeResult(t *testing.T) {
	b, _, ls, _ := newSendBackend(t)
	ls.execute = confirmAll
	events := make(chan Event, 100)
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{backend: b, emit: func(ev Event) { events <- ev }, ctx: ctx, cancel: cancel}
	defer e.Shutdown()
	conv := conversationID(testSelf, testPeer)
	for i := 0; i < 20; i++ {
		id := e.Call([]byte(`{"cmd":"send_text","args":{"conversation_id":"` + conv + `","text":"x","client_id":"c-` + strconv.Itoa(i) + `"}}`))
		var sawResponse bool
		for done := false; !done; {
			select {
			case ev := <-events:
				if ev.RequestID == nil || *ev.RequestID != id {
					continue
				}
				switch ev.Type {
				case EventResponse:
					if r := ev.Data.(Response); !r.OK {
						t.Fatalf("response = %+v", r.Error)
					}
					sawResponse = true
				case EventMessageSendResult:
					if !sawResponse {
						t.Fatalf("send %d: message_send_result before response", i)
					}
					done = true
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out")
			}
		}
	}
}

func TestSocketConfirmationWinsOverBrokenStream(t *testing.T) {
	confirmed := make(chan sendOutcome, 1)
	if o := confirmedOr(confirmed); o != outcomeNotConfirmed {
		t.Fatalf("empty = %+v", o)
	}
	confirmed <- sendOutcome{status: "sent", messageID: "mid.$1"}
	if o := confirmedOr(confirmed); o.status != "sent" || o.messageID != "mid.$1" {
		t.Fatalf("confirmed = %+v", o)
	}
	// End to end: a confirmation over the socket followed by a broken stream is reported as sent.
	b, log, ls, _ := newSendBackend(t)
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		otid := strconv.FormatInt(tasks[0].(*socket.SendMessageTask).Otid, 10)
		b.waits.observe(&table.LSTable{LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: otid, MessageId: "mid.$" + otid}}})
		return nil, errors.New("stream closed")
	}
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: conversationID(testSelf, testPeer), Text: "x", ClientID: "c-broken"}, log.emit); err != nil {
		t.Fatal(err)
	}
	if r := log.waitFor(t, EventMessageSendResult, 1)[0].Data.(MessageSendResult); r.Status != "sent" {
		t.Fatalf("result = %+v", r)
	}
}

func TestDuplicateClientIDRefusedWhileInFlight(t *testing.T) {
	b, log, ls, _ := newSendBackend(t)
	release := make(chan struct{})
	ls.execute = func(ctx context.Context, tasks []socket.Task) (*table.LSTable, error) {
		<-release
		return confirmAll(ctx, tasks)
	}
	conv := conversationID(testSelf, testPeer)
	args := SendTextArgs{ConversationID: conv, Text: "x", ClientID: "c-dup"}
	if err := b.SendText(context.Background(), args, log.emit); err != nil {
		t.Fatal(err)
	}
	var e *Error
	if err := b.SendText(context.Background(), args, log.emit); !errors.As(err, &e) || e.Code != "duplicate" {
		t.Fatalf("err = %v", err)
	}
	path := filepath.Join(t.TempDir(), "a.pdf")
	_ = os.WriteFile(path, []byte("%PDF"), 0600)
	if err := b.SendMedia(context.Background(), SendMediaArgs{ConversationID: conv, FilePath: path, ClientID: "c-dup"}, log.emit); !errors.As(err, &e) || e.Code != "duplicate" {
		t.Fatalf("media err = %v", err)
	}
	close(release)
	log.waitFor(t, EventMessageSendResult, 1)
	// Once the first send finished, the client_id may be retried.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := b.SendText(context.Background(), args, log.emit)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retry refused: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.waitFor(t, EventMessageSendResult, 2)
	if n := len(ls.calls()); n != 2 {
		t.Fatalf("LS calls = %d", n)
	}
}

func TestAddSendDoesNotReplaceWaiter(t *testing.T) {
	var w confirmWaits
	first, ok := w.addSend("1")
	if !ok {
		t.Fatal("first add refused")
	}
	if _, ok = w.addSend("1"); ok {
		t.Fatal("second add accepted")
	}
	w.observe(&table.LSTable{LSReplaceOptimsiticMessage: []*table.LSReplaceOptimsiticMessage{{OfflineThreadingId: "1", MessageId: "mid.$1"}}})
	if o := <-first; o.messageID != "mid.$1" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestFIFORejectedWithoutBlocking(t *testing.T) {
	b, log, _, _ := newSendBackend(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- b.SendMedia(context.Background(), SendMediaArgs{ConversationID: "u:200", FilePath: fifo, MIME: "application/pdf", ClientID: "c-fifo"}, log.emit)
	}()
	select {
	case err := <-done:
		var e *Error
		if !errors.As(err, &e) || e.Code != "file_unreadable" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		// Unblock the open so the goroutine ends, then fail.
		if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("reading a FIFO blocked")
	}
}

func TestE2EESentHookCalledOnSuccessOnly(t *testing.T) {
	b, log, _, wa := newSendBackend(t)
	type call struct {
		chat waTypes.JID
		id   string
	}
	calls := make(chan call, 4)
	b.onE2EESent = func(_ context.Context, chat waTypes.JID, id string, msg *waConsumerApplication.ConsumerApplication, _ *waMsgApplication.MessageApplication_Metadata, _ time.Time) {
		if msg == nil {
			t.Error("hook without message")
		}
		calls <- call{chat, id}
	}
	wa.send = func(_ context.Context, id string) (whatsmeow.SendResponse, error) {
		if id == waMessageID("c-bad") {
			return whatsmeow.SendResponse{}, whatsmeow.ErrServerReturnedError
		}
		return whatsmeow.SendResponse{ID: id, Timestamp: time.UnixMilli(7000)}, nil
	}
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "x", ClientID: "c-bad"}, log.emit)
	_ = b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "x", ClientID: "c-good"}, log.emit)
	log.waitFor(t, EventMessageSendResult, 2)
	select {
	case c := <-calls:
		if c.id != waMessageID("c-good") || c.chat.User != "200" {
			t.Fatalf("hook = %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook not called")
	}
	select {
	case c := <-calls:
		t.Fatalf("extra hook call %+v", c)
	case <-time.After(50 * time.Millisecond):
	}
}
