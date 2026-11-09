package engine

import (
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	testSelf  int64 = 1000
	testPeer  int64 = 100053372173569
	hybridKey int64 = 1147278207882860
)

type eventLog struct{ events []Event }

func (l *eventLog) emit(e Event) { l.events = append(l.events, e) }
func (l *eventLog) ofType(t string) []Event {
	var out []Event
	for _, e := range l.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func textWA(id string, chat, sender waTypes.JID, text string) *events.FBMessage {
	evt := &events.FBMessage{Message: consumerText(text)}
	evt.Info.ID = id
	evt.Info.Chat = chat
	evt.Info.Sender = sender
	evt.Info.Timestamp = time.UnixMilli(5000)
	return evt
}

func TestNormalAndE2EEThreadMapToSameConversation(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	idx.ApplyTable(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
		{ThreadKey: testPeer, ThreadType: table.ONE_TO_ONE, LastActivityTimestampMs: 1000},
	}, LSInsertMessage: []*table.LSInsertMessage{
		{ThreadKey: testPeer, MessageId: "mid.$old", SenderId: testPeer, TimestampMs: 900, Text: "cũ"},
	}}, log.emit)
	// Shape seen live: the E2EE thread has its own LS key and a hybrid mapping to the peer JID.
	idx.ApplyTable(&table.LSTable{
		LSVerifyHybridThreadExists: []*table.LSVerifyHybridThreadExists{
			{ThreadKey: hybridKey, ThreadJID: testPeer, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE},
		},
		LSUpdateOrInsertThread: []*table.LSUpdateOrInsertThread{
			{ThreadKey: hybridKey, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, LastActivityTimestampMs: 2000},
		},
	}, log.emit)
	peerJID := waTypes.NewJID("100053372173569", waTypes.MessengerServer)
	idx.ApplyWAMessage(textWA("wa1", peerJID, waTypes.JID{User: "100053372173569", Device: 2, Server: waTypes.MessengerServer}, "mới"), log.emit)

	want := "u:100053372173569"
	if convs := idx.Conversations(); len(convs) != 1 || convs[0].ID != want || convs[0].PeerID != "100053372173569" {
		t.Fatalf("conversations = %+v", convs)
	}
	for _, e := range log.ofType(EventConversationUpsert) {
		if e.Data.(Conversation).ID != want {
			t.Fatalf("unexpected conversation %+v", e.Data)
		}
	}
	messages := log.ofType(EventMessageUpsert)
	if len(messages) != 2 {
		t.Fatalf("messages = %d", len(messages))
	}
	ls, wa := messages[0].Data.(Message), messages[1].Data.(Message)
	if ls.ConversationID != want || ls.E2EE || ls.ThreadID != "100053372173569" {
		t.Fatalf("ls message = %+v", ls)
	}
	if wa.ConversationID != want || !wa.E2EE || wa.ThreadID != "100053372173569@msgr" {
		t.Fatalf("wa message = %+v", wa)
	}
}

func TestSendRoutePrefersE2EEAfterLink(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	idx.ApplyTable(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
		{ThreadKey: testPeer, ThreadType: table.ONE_TO_ONE, LastActivityTimestampMs: 1000},
	}}, log.emit)
	conv := conversationID(testSelf, testPeer)
	if thread, e2ee := idx.SendRoute(conv); thread != "100053372173569" || e2ee {
		t.Fatal(thread, e2ee)
	}
	idx.ApplyTable(&table.LSTable{LSCreateOpenToE2EEThreadLink: []*table.LSCreateOpenToE2EEThreadLink{
		{OpenThreadID: testPeer, ArmadilloThreadID: testPeer},
	}}, log.emit)
	if thread, e2ee := idx.SendRoute(conv); thread != "100053372173569@msgr" || !e2ee {
		t.Fatal(thread, e2ee)
	}
	if convs := idx.Conversations(); len(convs) != 1 {
		t.Fatalf("conversations = %+v", convs)
	}
	if thread, e2ee := idx.SendRoute("u:404"); thread != "" || e2ee {
		t.Fatal(thread, e2ee)
	}
}

func TestGroupThreadIgnored(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	const group int64 = 555000111
	idx.ApplyTable(&table.LSTable{
		LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
			{ThreadKey: group, ThreadType: table.GROUP_THREAD, ThreadName: "Nhóm"},
			{ThreadKey: group + 1, ThreadType: table.ENCRYPTED_OVER_WA_GROUP},
		},
		LSAddParticipantIdToGroupThread: []*table.LSAddParticipantIdToGroupThread{
			{ThreadKey: group, ContactId: testSelf}, {ThreadKey: group, ContactId: testPeer},
		},
		LSInsertMessage: []*table.LSInsertMessage{{ThreadKey: group, MessageId: "mid.$g", SenderId: testPeer, Text: "x"}},
		LSCreateOpenToE2EEThreadLink: []*table.LSCreateOpenToE2EEThreadLink{
			{OpenThreadID: group, ArmadilloThreadID: group + 1},
		},
	}, log.emit)
	groupJID := waTypes.NewJID("555000112", waTypes.GroupServer)
	evt := textWA("wag", groupJID, waTypes.NewJID("100053372173569", waTypes.MessengerServer), "x")
	evt.Info.IsGroup = true
	idx.ApplyWAMessage(evt, log.emit)
	if len(log.events) != 0 || len(idx.Conversations()) != 0 {
		t.Fatalf("events = %+v", log.events)
	}
}

func TestDuplicateMessageIDEmittedOnce(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	tbl := &table.LSTable{
		LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{{ThreadKey: testPeer, ThreadType: table.ONE_TO_ONE}},
		LSInsertMessage:          []*table.LSInsertMessage{{ThreadKey: testPeer, MessageId: "same-id", SenderId: testPeer, Text: "a"}},
	}
	idx.ApplyTable(tbl, log.emit)
	idx.ApplyTable(tbl, log.emit)
	peerJID := waTypes.NewJID("100053372173569", waTypes.MessengerServer)
	idx.ApplyWAMessage(textWA("same-id", peerJID, peerJID, "a"), log.emit)
	idx.ApplyWAMessage(textWA("wa-2", peerJID, peerJID, "b"), log.emit)
	idx.ApplyWAMessage(textWA("wa-2", peerJID, peerJID, "b"), log.emit)
	if got := len(log.ofType(EventMessageUpsert)); got != 2 {
		t.Fatalf("message_upsert = %d", got)
	}
}

func TestHybridMappingUsesThreadJIDAsPeer(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	const fbKey int64 = 7777000001
	idx.ApplyTable(&table.LSTable{
		LSUpdateThreadAuthorityAndMappingWithOTIDFromJID: []*table.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID{
			{ThreadKey: fbKey, ThreadJID: testPeer},
		},
		LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{{ThreadKey: fbKey, ThreadType: table.ONE_TO_ONE}},
		LSInsertMessage:          []*table.LSInsertMessage{{ThreadKey: fbKey, MessageId: "mid.$h", SenderId: testSelf, Text: "hi"}},
	}, log.emit)
	convs := idx.Conversations()
	if len(convs) != 1 || convs[0].ID != "u:100053372173569" || convs[0].PeerID != "100053372173569" {
		t.Fatalf("conversations = %+v", convs)
	}
	messages := log.ofType(EventMessageUpsert)
	if len(messages) != 1 {
		t.Fatalf("messages = %d", len(messages))
	}
	if m := messages[0].Data.(Message); m.ConversationID != "u:100053372173569" || !m.IsOutgoing {
		t.Fatalf("message = %+v", m)
	}
	if thread, e2ee := idx.SendRoute("u:100053372173569"); thread != "100053372173569@msgr" || !e2ee {
		t.Fatal(thread, e2ee)
	}
	// A later table that only references the FB key still resolves through the stored alias.
	idx.ApplyTable(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{
		{ThreadKey: fbKey, MessageId: "mid.$h2", SenderId: testPeer, Text: "yo"},
	}}, log.emit)
	if messages = log.ofType(EventMessageUpsert); len(messages) != 2 || messages[1].Data.(Message).ConversationID != "u:100053372173569" {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestE2EEOnlyConversationCreatedFromJID(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	peerJID := waTypes.NewJID("100053372173569", waTypes.MessengerServer)
	idx.ApplyWAMessage(textWA("wa1", peerJID, waTypes.JID{User: "100053372173569", Device: 3, Server: waTypes.MessengerServer}, "chào"), log.emit)
	if len(log.events) != 2 || log.events[0].Type != EventConversationUpsert || log.events[1].Type != EventMessageUpsert {
		t.Fatalf("events = %+v", log.events)
	}
	conv := log.events[0].Data.(Conversation)
	if conv.ID != "u:100053372173569" || conv.Name != "100053372173569" || conv.LastActivityTS != 5000 {
		t.Fatalf("conversation = %+v", conv)
	}
	if thread, e2ee := idx.SendRoute(conv.ID); thread != "100053372173569@msgr" || !e2ee {
		t.Fatal(thread, e2ee)
	}
	idx.ApplyTable(&table.LSTable{LSDeleteThenInsertContact: []*table.LSDeleteThenInsertContact{
		{Id: testPeer, Name: "Alice", ProfilePictureUrl: "https://example.invalid/a.jpg"},
	}}, log.emit)
	upserts := log.ofType(EventConversationUpsert)
	if len(upserts) != 2 {
		t.Fatalf("upserts = %d", len(upserts))
	}
	if c := upserts[1].Data.(Conversation); c.Name != "Alice" || c.AvatarURL == nil || *c.AvatarURL != "https://example.invalid/a.jpg" {
		t.Fatalf("conversation = %+v", c)
	}
	// An unchanged contact must not re-emit the conversation.
	idx.ApplyTable(&table.LSTable{LSDeleteThenInsertContact: []*table.LSDeleteThenInsertContact{
		{Id: testPeer, Name: "Alice", ProfilePictureUrl: "https://example.invalid/a.jpg"},
	}}, log.emit)
	if got := len(log.ofType(EventConversationUpsert)); got != 2 {
		t.Fatalf("upserts = %d", got)
	}
}

func TestE2EEThreadWithoutMappingWaitsForJID(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	idx.ApplyTable(&table.LSTable{LSUpdateOrInsertThread: []*table.LSUpdateOrInsertThread{
		{ThreadKey: hybridKey, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, LastActivityTimestampMs: 3000},
	}}, log.emit)
	if len(log.events) != 0 || len(idx.Conversations()) != 0 {
		t.Fatalf("thread key used as peer: %+v", log.events)
	}
	idx.ApplyTable(&table.LSTable{LSUpdateThreadAuthorityAndMappingWithOTIDFromJID: []*table.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID{
		{ThreadKey: hybridKey, ThreadJID: testPeer},
	}}, log.emit)
	convs := idx.Conversations()
	if len(convs) != 1 || convs[0].ID != "u:100053372173569" || convs[0].LastActivityTS != 3000 {
		t.Fatalf("conversations = %+v", convs)
	}
	if thread, e2ee := idx.SendRoute(convs[0].ID); thread != "100053372173569@msgr" || !e2ee {
		t.Fatal(thread, e2ee)
	}
}

func TestMappingWithoutTypeDoesNotCreateConversation(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	idx.ApplyTable(&table.LSTable{
		LSUpdateThreadAuthorityAndMappingWithOTIDFromJID: []*table.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID{
			{ThreadKey: hybridKey, ThreadJID: testPeer},
		},
		LSVerifyThreadExists: []*table.LSVerifyThreadExists{{ThreadKey: hybridKey, ThreadType: table.UNKNOWN_THREAD_TYPE}},
		LSInsertMessage:      []*table.LSInsertMessage{{ThreadKey: hybridKey, MessageId: "mid.$u", SenderId: testPeer, Text: "x"}},
	}, log.emit)
	if len(log.events) != 0 || len(idx.Conversations()) != 0 {
		t.Fatalf("events = %+v", log.events)
	}
	// A typed 1-1 row confirms the thread; the unknown-type row then only routes into it.
	idx.ApplyTable(&table.LSTable{LSUpdateOrInsertThread: []*table.LSUpdateOrInsertThread{
		{ThreadKey: hybridKey, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, LastActivityTimestampMs: 10},
	}}, log.emit)
	idx.ApplyTable(&table.LSTable{
		LSVerifyThreadExists: []*table.LSVerifyThreadExists{{ThreadKey: hybridKey}},
		LSInsertMessage:      []*table.LSInsertMessage{{ThreadKey: hybridKey, MessageId: "mid.$u2", SenderId: testPeer, Text: "y", TimestampMs: 20}},
	}, log.emit)
	if convs := idx.Conversations(); len(convs) != 1 || convs[0].ID != "u:100053372173569" {
		t.Fatalf("conversations = %+v", convs)
	}
	if got := len(log.ofType(EventMessageUpsert)); got != 1 {
		t.Fatalf("message_upsert = %d", got)
	}
	// Unknown type for a key never seen: no conversation.
	idx.ApplyTable(&table.LSTable{LSVerifyThreadExists: []*table.LSVerifyThreadExists{{ThreadKey: 424242}}}, log.emit)
	if len(idx.Conversations()) != 1 {
		t.Fatal("unknown thread created a conversation")
	}
}

func TestSpamAndPendingThreadsHidden(t *testing.T) {
	idx := NewThreadIndex(testSelf)
	var log eventLog
	const spammer, requester int64 = 200, 300
	idx.ApplyTable(&table.LSTable{
		LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
			{ThreadKey: spammer, ThreadType: table.ONE_TO_ONE, FolderName: "spam"},
			{ThreadKey: requester, ThreadType: table.ONE_TO_ONE, FolderName: "pending"},
		},
		LSInsertMessage: []*table.LSInsertMessage{
			{ThreadKey: spammer, MessageId: "mid.$s", SenderId: spammer, Text: "x"},
			{ThreadKey: requester, MessageId: "mid.$r", SenderId: requester, Text: "y"},
		},
	}, log.emit)
	if len(log.events) != 0 || len(idx.Conversations()) != 0 {
		t.Fatalf("events = %+v", log.events)
	}
	// Accepting the request moves the thread to the inbox; it then shows up.
	idx.ApplyTable(&table.LSTable{LSUpdateOrInsertThread: []*table.LSUpdateOrInsertThread{
		{ThreadKey: requester, ThreadType: table.ONE_TO_ONE, FolderName: "inbox"},
	}}, log.emit)
	if convs := idx.Conversations(); len(convs) != 1 || convs[0].ID != "u:300" {
		t.Fatalf("conversations = %+v", convs)
	}
}
