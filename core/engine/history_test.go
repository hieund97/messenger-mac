package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/securehistory"
	"go.mau.fi/mautrix-meta/pkg/messagix/securehistory/opaque"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waArmadilloBackupCommon"
	"go.mau.fi/whatsmeow/proto/waArmadilloBackupMessage"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// fakeHistory stands in for the secure history API.
type fakeHistory struct {
	mu       sync.Mutex
	backup   *securehistory.Backup // nil means ErrNoPINBackup
	enroll   func(pin []byte) (*securehistory.Device, error)
	fetch    func(ts int64, count int) (*securehistory.MessageRange, error)
	fetchTS  []int64
	uploads  chan *securehistory.MinosSentMessage
	pins     []string
	uploadEr error
}

func (f *fakeHistory) GetBackup(context.Context) (*securehistory.Backup, error) {
	if f.backup == nil {
		return nil, securehistory.ErrNoPINBackup
	}
	return f.backup, nil
}

func (f *fakeHistory) Enroll(_ context.Context, _ *securehistory.Backup, pin []byte) (*securehistory.Device, error) {
	f.mu.Lock()
	f.pins = append(f.pins, string(pin))
	f.mu.Unlock()
	return f.enroll(pin)
}

func (f *fakeHistory) FetchRange(_ context.Context, _ *securehistory.Device, _ string, ts int64, count int) (*securehistory.MessageRange, error) {
	f.mu.Lock()
	f.fetchTS = append(f.fetchTS, ts)
	f.mu.Unlock()
	return f.fetch(ts, count)
}

func (f *fakeHistory) UploadSent(_ context.Context, msg *securehistory.MinosSentMessage) error {
	if f.uploads != nil {
		f.uploads <- msg
	}
	return f.uploadEr
}

func testHistoryDevice() *securehistory.Device {
	return &securehistory.Device{ID: "dev-1", BackupID: "backup-1", MailboxRootKey: make([]byte, 32), StoragePublicKey: make([]byte, 32), StoragePrivateKey: make([]byte, 32), OCMFClientState: []byte{2},
		Epochs: map[string]securehistory.Epoch{"e1": {ID: "e1", AnonID: []byte("1"), RootKey: make([]byte, 32)}}}
}

// lsHistory builds the upsert a FetchMessagesTask answers with: messages newest first.
func lsHistory(thread int64, more bool, msgs ...[2]int64) *table.LSTable {
	tbl := &table.LSTable{}
	minTS, maxTS := msgs[len(msgs)-1][1], msgs[0][1]
	tbl.LSInsertNewMessageRange = []*table.LSInsertNewMessageRange{{
		ThreadKey: thread, MinTimestampMs: minTS, MinTimestampMsTemplate: minTS, MaxTimestampMs: maxTS, MaxTimestampMsTemplate: maxTS,
		MinMessageId: "m-" + strconv.FormatInt(msgs[len(msgs)-1][0], 10), MaxMessageId: "m-" + strconv.FormatInt(msgs[0][0], 10),
		HasMoreBefore: more,
	}}
	for _, m := range msgs {
		tbl.LSUpsertMessage = append(tbl.LSUpsertMessage, &table.LSUpsertMessage{ThreadKey: thread, MessageId: "m-" + strconv.FormatInt(m[0], 10),
			TimestampMs: m[1], SenderId: thread, Text: "old"})
	}
	return tbl
}

// newHistoryBackend has one normal 1-1 thread (testPeer) with message m-100 in memory.
func newHistoryBackend(t *testing.T) (*MetaBackend, *syncLog, *fakeLS) {
	t.Helper()
	b, log, ls, _ := newSendBackend(t)
	b.index.ApplyTable(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{
		{ThreadKey: testPeer, MessageId: "m-100", TimestampMs: 100_000, SenderId: testPeer, Text: "newest"},
	}}, func(Event) {})
	return b, log, ls
}

func fetchTasks(ls *fakeLS) []*socket.FetchMessagesTask {
	var out []*socket.FetchMessagesTask
	for _, call := range ls.calls() {
		for _, task := range call {
			if f, ok := task.(*socket.FetchMessagesTask); ok {
				out = append(out, f)
			}
		}
	}
	return out
}

func messageIDs(events []Event) []string {
	var ids []string
	for _, e := range events {
		if e.Type == EventMessageUpsert {
			ids = append(ids, e.Data.(Message).ID)
		}
	}
	return ids
}

func fetchData(t *testing.T, raw json.RawMessage) (hasMore bool, cursor string, data map[string]any) {
	t.Helper()
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	hasMore, _ = data["has_more"].(bool)
	cursor, _ = data["cursor"].(string)
	return hasMore, cursor, data
}

// runFetch calls FetchMessages with a command emitter and returns its events and response data.
func runFetch(t *testing.T, b *MetaBackend, args FetchMessagesArgs) ([]Event, json.RawMessage) {
	t.Helper()
	var events []Event
	raw, err := b.fetchMessages(context.Background(), args, func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	return events, raw
}

func TestFetchPagingStopsWhenNoMore(t *testing.T) {
	b, _, ls := newHistoryBackend(t)
	b.pagingDelay = 10 * time.Millisecond
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		f := tasks[0].(*socket.FetchMessagesTask)
		switch f.ReferenceMessageId {
		case "m-100":
			// The anchor itself comes back too and must not be emitted again.
			return lsHistory(testPeer, true, [2]int64{100, 100_000}, [2]int64{99, 99_000}, [2]int64{98, 98_000}), nil
		case "m-98":
			return lsHistory(testPeer, false, [2]int64{97, 97_000}), nil
		}
		t.Errorf("unexpected reference %q", f.ReferenceMessageId)
		return &table.LSTable{}, nil
	}
	conv := conversationID(testSelf, testPeer)
	events, raw := runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Limit: 2})
	if ids := messageIDs(events); strings.Join(ids, ",") != "m-99,m-98" {
		t.Fatalf("first page = %v", ids)
	}
	more, cursor, _ := fetchData(t, raw)
	if !more || cursor == "" {
		t.Fatalf("first page has_more=%v cursor=%q", more, cursor)
	}
	events, raw = runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Cursor: &cursor, Limit: 2})
	if ids := messageIDs(events); strings.Join(ids, ",") != "m-97" {
		t.Fatalf("second page = %v", ids)
	}
	if more, _, _ = fetchData(t, raw); more {
		t.Fatal("has_more after the server reported no more")
	}
	tasks := fetchTasks(ls)
	if len(tasks) != 2 || tasks[0].ReferenceTimestampMs != 100_000 || tasks[1].ReferenceTimestampMs != 98_000 || tasks[0].ThreadKey != testPeer {
		t.Fatalf("tasks = %+v", tasks)
	}
	// A message of the fetched pages arriving live again is a duplicate.
	var live []Event
	b.index.ApplyTable(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{
		{ThreadKey: testPeer, MessageId: "m-97", TimestampMs: 97_000, SenderId: testPeer, Text: "old"},
	}}, func(e Event) { live = append(live, e) })
	if len(messageIDs(live)) != 0 {
		t.Fatalf("fetched message emitted again: %v", live)
	}
}

func TestFetchBatchesSpacedAtLeast1s(t *testing.T) {
	b, _, ls := newHistoryBackend(t)
	var mu sync.Mutex
	var at []time.Time
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		f := tasks[0].(*socket.FetchMessagesTask)
		if f.ReferenceMessageId == "m-100" {
			return lsHistory(testPeer, true, [2]int64{99, 99_000}, [2]int64{98, 98_000}), nil
		}
		return lsHistory(testPeer, true, [2]int64{97, 97_000}, [2]int64{96, 96_000}), nil
	}
	events, raw := runFetch(t, b, FetchMessagesArgs{ConversationID: conversationID(testSelf, testPeer), Limit: 3})
	if ids := messageIDs(events); strings.Join(ids, ",") != "m-99,m-98,m-97,m-96" {
		t.Fatalf("messages = %v (must be newest first)", ids)
	}
	if more, _, _ := fetchData(t, raw); !more {
		t.Fatal("has_more = false")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 2 {
		t.Fatalf("%d batches", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < time.Second-5*time.Millisecond {
		t.Fatalf("batches %v apart", gap)
	}
}

// fakePager serves pages of thread tables per sync group. frontier is the oldest activity each group
// has reached; a served page moves it to the oldest thread row of the page.
type fakePager struct {
	mu       sync.Mutex
	pages    map[int64][]*table.LSTable
	frontier map[int64]int64
	calls    []int64
}

func (p *fakePager) Frontier(group int64) (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frontier[group], len(p.pages[group]) > 0
}

func (p *fakePager) FetchThreadPage(_ context.Context, group int64, handle func(*table.LSTable)) (bool, error) {
	p.mu.Lock()
	p.calls = append(p.calls, group)
	pages := p.pages[group]
	if len(pages) == 0 {
		p.mu.Unlock()
		return false, nil
	}
	page := pages[0]
	p.pages[group] = pages[1:]
	more := len(p.pages[group]) > 0
	for _, t := range page.LSDeleteThenInsertThread {
		p.frontier[group] = min(p.frontier[group], t.LastActivityTimestampMs)
	}
	for _, t := range page.LSVerifyHybridThreadExists {
		p.frontier[group] = min(p.frontier[group], t.LastActivityTimestampMS)
	}
	p.mu.Unlock()
	handle(page)
	return more, nil
}

func threadRow(key int64, typ table.ThreadType, ts int64) *table.LSDeleteThenInsertThread {
	return &table.LSDeleteThenInsertThread{ThreadKey: key, ThreadType: typ, LastActivityTimestampMs: ts}
}

func TestFetchConversationsPaging(t *testing.T) {
	b, _, _, _ := newSendBackend(t)
	b.pagingDelay = 10 * time.Millisecond
	pager := &fakePager{pages: map[int64][]*table.LSTable{
		1: {
			{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{threadRow(301, table.ONE_TO_ONE, 900), threadRow(9001, table.GROUP_THREAD, 899)}},
			{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{threadRow(302, table.ONE_TO_ONE, 800)}},
		},
		95: {
			{LSVerifyHybridThreadExists: []*table.LSVerifyHybridThreadExists{{ThreadKey: hybridKey, ThreadJID: 303, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, LastActivityTimestampMS: 700}}},
		},
	}, frontier: map[int64]int64{1: 1000, 95: 750}}
	b.pagerOverride = pager
	call := func(cursor *string) ([]string, bool, string) {
		var convs []string
		raw, err := b.fetchConversations(context.Background(), FetchConversationsArgs{Cursor: cursor}, func(e Event) {
			if e.Type == EventConversationUpsert {
				convs = append(convs, e.Data.(Conversation).ID)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		more, next, _ := fetchData(t, raw)
		return convs, more, next
	}
	convs, more, cursor := call(nil)
	if strings.Join(convs, ",") != "u:301" || !more || cursor == "" {
		t.Fatalf("page 1: %v more=%v cursor=%q", convs, more, cursor)
	}
	convs, more, cursor = call(&cursor)
	if strings.Join(convs, ",") != "u:302" || !more {
		t.Fatalf("page 2: %v more=%v", convs, more)
	}
	convs, more, _ = call(&cursor)
	if strings.Join(convs, ",") != "u:303" || more {
		t.Fatalf("page 3: %v more=%v", convs, more)
	}
	pager.mu.Lock()
	defer pager.mu.Unlock()
	if len(pager.calls) != 3 || pager.calls[0] != 1 || pager.calls[1] != 1 || pager.calls[2] != 95 {
		t.Fatalf("calls = %v", pager.calls)
	}
}

// TestFetchConversationsMergesGroupsByActivity: the E2EE list is paged as soon as it reaches more
// recent activity than the normal list, instead of after the normal list ends (seen live: E2EE chats
// older than the startup pages never appeared while years of normal threads did).
func TestFetchConversationsMergesGroupsByActivity(t *testing.T) {
	b, _, _, _ := newSendBackend(t)
	b.pagingDelay = time.Millisecond
	hybrid := func(key, peer, ts int64) *table.LSTable {
		return &table.LSTable{LSVerifyHybridThreadExists: []*table.LSVerifyHybridThreadExists{{ThreadKey: key, ThreadJID: peer, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, LastActivityTimestampMS: ts}}}
	}
	pager := &fakePager{pages: map[int64][]*table.LSTable{
		1: {
			{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{threadRow(301, table.ONE_TO_ONE, 900)}},
			{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{threadRow(302, table.ONE_TO_ONE, 500)}},
			{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{threadRow(305, table.ONE_TO_ONE, 100)}},
		},
		95: {hybrid(7001, 303, 800), hybrid(7002, 304, 400)},
	}, frontier: map[int64]int64{1: 1000, 95: 950}}
	b.pagerOverride = pager
	var cursor *string
	var order []string
	for page := 0; page < 10; page++ {
		raw, err := b.fetchConversations(context.Background(), FetchConversationsArgs{Cursor: cursor}, func(e Event) {
			if e.Type == EventConversationUpsert {
				order = append(order, e.Data.(Conversation).ID)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		more, next, _ := fetchData(t, raw)
		if !more {
			break
		}
		cursor = &next
	}
	if got := strings.Join(order, ","); got != "u:301,u:303,u:302,u:304,u:305" {
		t.Fatalf("order = %s", got)
	}
}

func historyStates(log *syncLog) []HistoryRestoreState {
	var out []HistoryRestoreState
	for _, e := range log.ofType(EventHistoryRestoreState) {
		out = append(out, e.Data.(HistoryRestoreState))
	}
	return out
}

func TestWrongPinEmitsFailedWrongPin(t *testing.T) {
	b, log, _, _ := newSendBackend(t)
	remaining := 4
	h := &fakeHistory{backup: &securehistory.Backup{ID: "backup-1"}, enroll: func([]byte) (*securehistory.Device, error) {
		return nil, &securehistory.RecoveryError{Code: "wrong_pin", AttemptsRemaining: &remaining}
	}}
	b.historyOverride = h
	if err := b.RestoreHistory(context.Background(), RestoreHistoryArgs{PIN: "111111"}, log.emit); err != nil {
		t.Fatal(err)
	}
	states := historyStates(log)
	if len(states) != 2 || states[0].State != "in_progress" || states[1].State != "failed" || states[1].Reason == nil ||
		*states[1].Reason != "wrong_pin" || states[1].AttemptsRemaining == nil || *states[1].AttemptsRemaining != 4 {
		t.Fatalf("states = %+v", states)
	}
	// A malformed PIN is wrong without asking the server.
	if err := b.RestoreHistory(context.Background(), RestoreHistoryArgs{PIN: "12a"}, log.emit); err != nil {
		t.Fatal(err)
	}
	states = historyStates(log)
	if last := states[len(states)-1]; last.State != "failed" || *last.Reason != "wrong_pin" || len(h.pins) != 1 {
		t.Fatalf("states = %+v, server calls %d", states, len(h.pins))
	}
	// No backup at all.
	h.backup = nil
	_ = b.RestoreHistory(context.Background(), RestoreHistoryArgs{PIN: "222222"}, log.emit)
	states = historyStates(log)
	if last := states[len(states)-1]; last.State != "failed" || *last.Reason != "no_backup" {
		t.Fatalf("states = %+v", states)
	}
	// The fake backend used by the UI: 123456 is right, anything else wrong.
	fake := newSyncLog()
	_ = FakeBackend{}.RestoreHistory(context.Background(), RestoreHistoryArgs{PIN: "000000"}, fake.emit)
	if s := historyStates(fake); len(s) == 0 || s[len(s)-1].State != "failed" || *s[len(s)-1].Reason != "wrong_pin" {
		t.Fatalf("fake states = %+v", s)
	}
}

func TestPinNeverWrittenToDisk(t *testing.T) {
	dir := t.TempDir()
	b := NewMetaBackend(Config{DataDir: dir})
	b.newClient = func(*cookies.Cookies, zerolog.Logger) metaClient { return &testMetaClient{} }
	log := newSyncLog()
	if err := b.Login(context.Background(), validArgs("messenger"), log.emit); err != nil {
		t.Fatal(err)
	}
	const wrongPIN, rightPIN = "482913", "735102"
	b.historyOverride = &fakeHistory{backup: &securehistory.Backup{ID: "backup-1"}, enroll: func(pin []byte) (*securehistory.Device, error) {
		if string(pin) != rightPIN {
			return nil, &securehistory.RecoveryError{Code: "wrong", AttemptsRemaining: new(int)}
		}
		return testHistoryDevice(), nil
	}}
	for _, pin := range []string{wrongPIN, rightPIN} {
		if err := b.RestoreHistory(context.Background(), RestoreHistoryArgs{PIN: pin}, log.emit); err != nil {
			t.Fatal(err)
		}
	}
	if s := historyStates(log); len(s) == 0 || s[len(s)-1].State != "done" {
		t.Fatalf("states = %+v", s)
	}
	secrets := log.ofType(EventSecretStore)
	if len(secrets) == 0 {
		t.Fatal("no secret_store after restore")
	}
	for _, e := range secrets {
		if s := e.Data.(SecretStore); strings.Contains(s.Value, rightPIN) || strings.Contains(s.Value, wrongPIN) {
			t.Fatal("PIN inside a secret")
		}
	}
	b.stop()
	if _, err := os.Stat(filepath.Join(dir, "1", "core.log")); err != nil {
		t.Fatal(err)
	}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), rightPIN) || strings.Contains(string(data), wrongPIN) {
			t.Errorf("PIN written to %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoginWithSecretsSkipsNeeded(t *testing.T) {
	device, err := json.Marshal(historySecret{UserID: "1", Device: testHistoryDevice()})
	if err != nil {
		t.Fatal(err)
	}
	for _, withSecrets := range []bool{false, true} {
		b := NewMetaBackend(Config{})
		client := &testMetaClient{}
		b.newClient = func(*cookies.Cookies, zerolog.Logger) metaClient { return client }
		log := newSyncLog()
		args := validArgs("messenger")
		if withSecrets {
			args.Secrets = map[string]string{SecretSecureHistoryDevice: string(device)}
		}
		if err = b.Login(context.Background(), args, log.emit); err != nil {
			t.Fatal(err)
		}
		b.handleEvent(client, &messagix.ConnectedEvent{})
		b.handleEvent(client, &messagix.ReconnectedEvent{})
		states := historyStates(log)
		want := "needed"
		if withSecrets {
			want = "done"
		}
		if len(states) != 1 || states[0].State != want {
			t.Fatalf("secrets=%v states=%+v", withSecrets, states)
		}
		b.stop()
	}
}

func TestSentE2EEMessageQueuedForUpload(t *testing.T) {
	b, log, _, wa := newSendBackend(t)
	b.cookies = &cookies.Cookies{Platform: types.Messenger}
	h := &fakeHistory{backup: &securehistory.Backup{ID: "backup-9"}, uploads: make(chan *securehistory.MinosSentMessage, 4),
		uploadEr: errors.New("upload failed")}
	b.historyOverride = h
	if err := b.SendText(context.Background(), SendTextArgs{ConversationID: "u:200", Text: "hi", ClientID: "c-up"}, log.emit); err != nil {
		t.Fatal(err)
	}
	log.waitFor(t, EventMessageSendResult, 1)
	if r := sendResults(log)[0]; r.Status != "sent" {
		t.Fatalf("result = %+v (an upload failure must not change it)", r)
	}
	select {
	case m := <-h.uploads:
		id := waMessageID("c-up")
		otid, _ := strconv.ParseInt(id, 10, 64)
		if m.MessageID != id || m.BackupID != "backup-9" || m.OtherUserID != testE2EEPeer || m.SenderID != testSelf ||
			m.TimestampMS != otid>>22 || len(m.TransportBytes) == 0 || len(m.FrankingTag) != 32 {
			t.Fatalf("upload = %+v", m)
		}
		var app waMsgApplication.MessageApplication
		if err := proto.Unmarshal(m.TransportBytes, &app); err != nil || len(app.GetMetadata().GetFrankingKey()) != 32 {
			t.Fatalf("transport bytes: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sent message not uploaded")
	}
	wa.mu.Lock()
	defer wa.mu.Unlock()
	if len(wa.sends) != 1 {
		t.Fatalf("sends = %v", wa.sends)
	}
}

// archived builds an unencrypted secure history entry (the decrypt path that needs no keys).
func archived(t *testing.T, id, sender, text string, ts int64) *securehistory.ArchiveMessage {
	t.Helper()
	app, err := proto.Marshal(&waMsgApplication.MessageApplication{Payload: &waMsgApplication.MessageApplication_Payload{
		Content: &waMsgApplication.MessageApplication_Payload_SubProtocol{SubProtocol: &waMsgApplication.MessageApplication_SubProtocolPayload{
			SubProtocol: &waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage{ConsumerMessage: &waCommon.SubProtocol{
				Payload: mustMarshal(t, consumerText(text)), Version: proto.Int32(whatsmeow.FBConsumerMessageVersion),
			}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := proto.Marshal(&waArmadilloBackupMessage.BackupMessage{
		Metadata: &waArmadilloBackupCommon.Metadata{SenderID: proto.String(sender), MessageID: proto.String(id), TimestampMS: proto.Int64(ts)},
		Payload:  &waArmadilloBackupMessage.BackupMessage_EncryptedTransportMessage{EncryptedTransportMessage: app},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &securehistory.ArchiveMessage{ID: id, Protobufs: &securehistory.ArchiveProtobufs{}}
	m.Protobufs.Unencrypted = &struct {
		Message     []byte `json:"unencrypted_protobuf"`
		TimestampMS int64  `json:"protobuf_timestamp_ms,string"`
	}{Message: backup, TimestampMS: ts}
	return m
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetchE2EEIncludesSameTimestampWithoutDuplicates(t *testing.T) {
	b, _, _, _ := newSendBackend(t)
	b.pagingDelay = 10 * time.Millisecond
	// u:200 is E2EE-only; wa-in-1 (ts 5000, from newSendBackend) is in memory.
	conv := "u:200"
	var cursor string
	_, raw := runFetch(t, b, FetchMessagesArgs{ConversationID: conv})
	if more, _, data := fetchData(t, raw); more || data["needs_restore"] != true {
		t.Fatalf("before restore: %v", data)
	}
	b.history.setDevice(testHistoryDevice())
	peer := strconv.FormatInt(testE2EEPeer, 10)
	no, yes := false, true
	h := &fakeHistory{fetch: func(ts int64, _ int) (*securehistory.MessageRange, error) {
		if ts == 5001 {
			return &securehistory.MessageRange{Messages: []*securehistory.ArchiveMessage{
				archived(t, "wa-in-1", peer, "a", 5000), archived(t, "wa-old-2", peer, "same ts", 5000), archived(t, "wa-old-1", peer, "older", 4000),
			}, Range: &securehistory.MessageRangeInfo{HasMoreBefore: &yes, HasMoreAfter: &no, Before: 3999}}, nil
		}
		return &securehistory.MessageRange{Messages: []*securehistory.ArchiveMessage{archived(t, "wa-old-0", peer, "oldest", 3000)},
			Range: &securehistory.MessageRangeInfo{HasMoreBefore: &no, HasMoreAfter: &yes}}, nil
	}}
	b.historyOverride = h
	events, raw := runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Limit: 2})
	if ids := messageIDs(events); strings.Join(ids, ",") != "wa-old-2,wa-old-1" {
		t.Fatalf("page 1 = %v", ids)
	}
	for _, e := range events {
		if m := e.Data.(Message); !m.E2EE || m.ConversationID != conv || m.ThreadID != waTypes.NewJID(peer, waTypes.MessengerServer).String() {
			t.Fatalf("message = %+v", m)
		}
	}
	more, cursor, _ := fetchData(t, raw)
	if !more {
		t.Fatal("has_more = false")
	}
	events, raw = runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Cursor: &cursor, Limit: 2})
	if ids := messageIDs(events); strings.Join(ids, ",") != "wa-old-0" {
		t.Fatalf("page 2 = %v", ids)
	}
	if more, _, _ = fetchData(t, raw); more {
		t.Fatal("has_more after the end")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.fetchTS) != 2 || h.fetchTS[0] != 5001 || h.fetchTS[1] != 4001 {
		t.Fatalf("reference timestamps = %v", h.fetchTS)
	}
}

// TestFetchPagesBothThreadsByTime: a conversation with a normal and an E2EE thread pages whichever
// thread is at the more recent position, so one page never holds old normal messages above E2EE
// messages that were not fetched yet (seen live: page 1 went back to 2024 in the normal thread while
// the E2EE thread was still in 2026). count reports the emitted messages.
func TestFetchPagesBothThreadsByTime(t *testing.T) {
	b, _, ls := newHistoryBackend(t)
	b.pagingDelay = time.Millisecond
	b.index.ApplyTable(&table.LSTable{LSCreateOpenToE2EEThreadLink: []*table.LSCreateOpenToE2EEThreadLink{
		{OpenThreadID: testPeer, ArmadilloThreadID: testPeer},
	}}, func(Event) {})
	b.history.setDevice(testHistoryDevice())
	peer := strconv.FormatInt(testPeer, 10)
	no, yes := false, true
	b.historyOverride = &fakeHistory{fetch: func(ts int64, _ int) (*securehistory.MessageRange, error) {
		if ts > 200_001 {
			return &securehistory.MessageRange{Messages: []*securehistory.ArchiveMessage{
				archived(t, "wa-3", peer, "c", 300_000), archived(t, "wa-2", peer, "b", 200_000),
			}, Range: &securehistory.MessageRangeInfo{HasMoreBefore: &yes, HasMoreAfter: &no, Before: 199_999}}, nil
		}
		return &securehistory.MessageRange{Messages: []*securehistory.ArchiveMessage{archived(t, "wa-1", peer, "a", 70_000)},
			Range: &securehistory.MessageRangeInfo{HasMoreBefore: &no, HasMoreAfter: &yes}}, nil
	}}
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		if f := tasks[0].(*socket.FetchMessagesTask); f.ReferenceMessageId == "m-100" {
			return lsHistory(testPeer, true, [2]int64{50, 50_000}, [2]int64{49, 49_000}), nil
		}
		return lsHistory(testPeer, false, [2]int64{48, 48_000}), nil
	}
	conv := conversationID(testSelf, testPeer)
	events, raw := runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Limit: 2})
	more, cursor, data := fetchData(t, raw)
	if ids := strings.Join(messageIDs(events), ","); ids != "wa-3,wa-2" || !more || data["count"] != float64(2) {
		t.Fatalf("page 1 = %s more=%v data=%v", ids, more, data)
	}
	events, raw = runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Cursor: &cursor, Limit: 2})
	more, cursor, data = fetchData(t, raw)
	if ids := strings.Join(messageIDs(events), ","); ids != "wa-1,m-50,m-49" || !more || data["count"] != float64(3) {
		t.Fatalf("page 2 = %s more=%v data=%v", ids, more, data)
	}
	events, raw = runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Cursor: &cursor, Limit: 2})
	more, _, data = fetchData(t, raw)
	if ids := strings.Join(messageIDs(events), ","); ids != "m-48" || more || data["count"] != float64(1) {
		t.Fatalf("page 3 = %s more=%v data=%v", ids, more, data)
	}
}

// TestFetchHoldsBackMessagesBehindOtherThread: in a chat moved to E2EE, the normal thread (no message
// in memory) answers with its last messages from long ago. They are held back until the E2EE thread
// has paged down to them, then come again from the anchor message.
func TestFetchHoldsBackMessagesBehindOtherThread(t *testing.T) {
	b, _, ls, _ := newSendBackend(t)
	b.pagingDelay = time.Millisecond
	b.index.ApplyTable(&table.LSTable{LSCreateOpenToE2EEThreadLink: []*table.LSCreateOpenToE2EEThreadLink{
		{OpenThreadID: testPeer, ArmadilloThreadID: testPeer},
	}}, func(Event) {})
	b.history.setDevice(testHistoryDevice())
	peer := strconv.FormatInt(testPeer, 10)
	no, yes := false, true
	b.historyOverride = &fakeHistory{fetch: func(ts int64, _ int) (*securehistory.MessageRange, error) {
		if ts > 200_001 {
			return &securehistory.MessageRange{Messages: []*securehistory.ArchiveMessage{
				archived(t, "wa-3", peer, "c", 300_000), archived(t, "wa-2", peer, "b", 200_000),
			}, Range: &securehistory.MessageRangeInfo{HasMoreBefore: &yes, HasMoreAfter: &no, Before: 199_999}}, nil
		}
		return &securehistory.MessageRange{Messages: []*securehistory.ArchiveMessage{archived(t, "wa-1", peer, "a", 70_000)},
			Range: &securehistory.MessageRangeInfo{HasMoreBefore: &no, HasMoreAfter: &yes}}, nil
	}}
	var refs []string
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		switch task := tasks[0].(type) {
		case *socket.CreateThreadTask:
			refs = append(refs, "create")
			return lsHistory(testPeer, true, [2]int64{50, 50_000}, [2]int64{49, 49_000}), nil
		case *socket.FetchMessagesTask:
			refs = append(refs, task.ReferenceMessageId)
			return lsHistory(testPeer, false, [2]int64{50, 50_000}, [2]int64{49, 49_000}, [2]int64{48, 48_000}), nil
		}
		return &table.LSTable{}, nil
	}
	conv := conversationID(testSelf, testPeer)
	events, raw := runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Limit: 2})
	more, cursor, data := fetchData(t, raw)
	if ids := strings.Join(messageIDs(events), ","); ids != "wa-3,wa-2" || !more || data["count"] != float64(2) {
		t.Fatalf("page 1 = %s more=%v data=%v", ids, more, data)
	}
	events, raw = runFetch(t, b, FetchMessagesArgs{ConversationID: conv, Cursor: &cursor, Limit: 2})
	more, _, data = fetchData(t, raw)
	if ids := strings.Join(messageIDs(events), ","); ids != "wa-1,m-50,m-49,m-48" || more || data["count"] != float64(4) {
		t.Fatalf("page 2 = %s more=%v data=%v", ids, more, data)
	}
	if strings.Join(refs, ",") != "create,m-50" {
		t.Fatalf("normal thread requests = %v", refs)
	}
}

func TestHistorySecretAccountAndValidation(t *testing.T) {
	dev := testHistoryDevice()
	event, err := deviceSecret(dev, 1)
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{SecretSecureHistoryDevice: event.Data.(SecretStore).Value}
	if deviceFromSecrets(secrets, 1) == nil || deviceFromSecrets(secrets, 2) != nil {
		t.Fatal("account binding failed")
	}
	dev.StoragePrivateKey = nil
	event, _ = deviceSecret(dev, 1)
	secrets[SecretSecureHistoryDevice] = event.Data.(SecretStore).Value
	if deviceFromSecrets(secrets, 1) != nil {
		t.Fatal("accepted incomplete device")
	}
}

func TestRestoreFailureReasons(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{&securehistory.RecoveryError{AttemptsRemaining: &zero}, "locked"},
		{&securehistory.RecoveryError{}, "wrong_pin"},
		{fmt.Errorf("recover: %w", opaque.ErrInvalidPIN), "wrong_pin"},
		{types.ErrPleaseLogIn, "auth_required"},
		{httpclient.ErrTokenInvalidated, "auth_required"},
	} {
		reason, attempts := restoreFailure(tc.err)
		if reason != tc.reason || (reason == "locked" && (attempts == nil || *attempts != 0)) {
			t.Fatalf("%v: %s %v", tc.err, reason, attempts)
		}
	}
}

func TestInvalidHistoryRequiresRestore(t *testing.T) {
	for _, failure := range []string{"archive is no longer available", "different backup", "archive device is not enrolled", "backup epoch source mismatch"} {
		t.Run(failure, func(t *testing.T) {
			b, log, _, _ := newSendBackend(t)
			b.history.setDevice(testHistoryDevice())
			b.index.SetHistoryRestored(true, nil)
			b.historyOverride = &fakeHistory{fetch: func(int64, int) (*securehistory.MessageRange, error) { return nil, errors.New(failure) }, backup: &securehistory.Backup{ID: "backup-1"}, enroll: func([]byte) (*securehistory.Device, error) { return testHistoryDevice(), nil }}
			_, err := b.fetchMessages(context.Background(), FetchMessagesArgs{ConversationID: "u:200"}, log.emit)
			if !isNeedsRestore(err) || b.history.device() != nil {
				t.Fatalf("err=%v device=%v", err, b.history.device())
			}
			secrets := log.ofType(EventSecretStore)
			states := historyStates(log)
			if len(secrets) != 1 || secrets[0].Data.(SecretStore).Value != "" || len(states) == 0 || states[len(states)-1].State != "needed" {
				t.Fatalf("events: %v %v", secrets, states)
			}
			if err := b.RestoreHistory(context.Background(), RestoreHistoryArgs{PIN: "123456"}, log.emit); err != nil || b.history.device() == nil {
				t.Fatalf("restore: %v", err)
			}
		})
	}
}

func TestLSBatchRetriesOnce(t *testing.T) {
	b, _, ls := newHistoryBackend(t)
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		if len(fetchTasks(ls)) == 2 {
			return lsHistory(testPeer, false, [2]int64{99, 99000}), nil
		}
		return nil, nil
	}
	_, _ = runFetch(t, b, FetchMessagesArgs{ConversationID: conversationID(testSelf, testPeer)})
	if n := len(fetchTasks(ls)); n != 2 {
		t.Fatalf("tasks=%d", n)
	}
}

func TestStopClearsHistoryDevice(t *testing.T) {
	b, _, _, _ := newSendBackend(t)
	b.history.setDevice(testHistoryDevice())
	b.stop()
	if b.history.device() != nil {
		t.Fatal("device survived stop")
	}
}

func TestEpochsUseCurrentDevice(t *testing.T) {
	b, _, _, _ := newSendBackend(t)
	old := testHistoryDevice()
	current := testHistoryDevice()
	current.Epochs["extra"] = securehistory.Epoch{ID: "extra", AnonID: []byte("2"), RootKey: make([]byte, 32)}
	b.history.setDevice(current)
	got, err := b.applyHistoryEpochs(old, nil)
	if err != nil || got.Epochs["extra"].ID != "extra" {
		t.Fatalf("device=%v err=%v", got, err)
	}
}

func TestFetchSessionCancellationIsDisconnected(t *testing.T) {
	b, _, ls := newHistoryBackend(t)
	b.ctx, b.cancel = context.WithCancel(context.Background())
	ls.execute = func(ctx context.Context, _ []socket.Task) (*table.LSTable, error) {
		b.cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := b.fetchMessages(context.Background(), FetchMessagesArgs{ConversationID: conversationID(testSelf, testPeer)}, func(Event) {})
	var e *Error
	if !errors.As(err, &e) || e.Code != "disconnected" {
		t.Fatalf("err=%v", err)
	}
}
