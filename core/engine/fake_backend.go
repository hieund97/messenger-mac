package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FakeBackend serves canned data so the Swift UI can run without an account: two conversations (Bob's
// is E2EE), paged history, more conversations to page through and a PIN restore where 123456 is right.
type FakeBackend struct{ st *fakeState }

type fakeState struct {
	mu       sync.Mutex
	base     int64 // "now" of the fake history, fixed so pages line up across calls
	restored bool
}

const (
	fakeHistoryLen   = 60 // older messages per conversation
	fakeConvPages    = 2
	fakeConvPageSize = 10
	fakePIN          = "123456"
	fakeE2EEConv     = "u:2"
)

func NewFakeBackend() FakeBackend { return FakeBackend{st: &fakeState{base: time.Now().UnixMilli()}} }

// state tolerates the zero FakeBackend{} (stateless: not restored, history ending now).
func (f FakeBackend) state() *fakeState {
	if f.st == nil {
		return &fakeState{base: time.Now().UnixMilli()}
	}
	return f.st
}

func (s *fakeState) isRestored() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restored
}

func fakeConversations(restored bool, now int64) []Conversation {
	return []Conversation{
		{ID: "u:1", PeerID: "1", Name: "Alice", LastActivityTS: now - 1000, HistoryRestored: true},
		{ID: fakeE2EEConv, PeerID: "2", Name: "Bob", LastActivityTS: now, HistoryRestored: restored},
	}
}

func (f FakeBackend) Login(_ context.Context, args LoginArgs, emit func(Event)) error {
	st := f.state()
	st.mu.Lock()
	st.restored = args.Secrets[SecretSecureHistoryDevice] != ""
	restored := st.restored
	st.mu.Unlock()
	now := time.Now().UnixMilli()
	emit(Event{Type: EventConnectionState, Data: ConnectionState{State: "connected"}})
	for _, c := range fakeConversations(restored, now) {
		emit(Event{Type: EventConversationUpsert, Data: c})
	}
	for i, msg := range []struct{ id, conv, text string }{{"1", "u:1", "Hello"}, {"2", "u:1", "Hi"}, {"3", fakeE2EEConv, "Hello"}} {
		text := msg.text
		m := Message{ID: msg.id, ConversationID: msg.conv, ThreadID: msg.conv, SenderID: msg.conv[2:], TS: now - int64(2-i)*1000, Text: &text, Attachments: []Attachment{}}
		if msg.conv == fakeE2EEConv {
			m.ThreadID, m.E2EE = "2@msgr", true
		}
		emit(Event{Type: EventMessageUpsert, Data: m})
	}
	state := "needed"
	if restored {
		state = "done"
	}
	emit(historyEvent(state, "", nil))
	return nil
}

func (f FakeBackend) RestoreHistory(_ context.Context, args RestoreHistoryArgs, emit func(Event)) error {
	st := f.state()
	if !historyPIN.MatchString(args.PIN) {
		emit(historyEvent("failed", "wrong_pin", nil))
		return nil
	}
	emit(historyEvent("in_progress", "", nil))
	if args.PIN != fakePIN {
		attempts := 4
		emit(historyEvent("failed", "wrong_pin", &attempts))
		return nil
	}
	st.mu.Lock()
	st.restored = true
	st.mu.Unlock()
	emit(Event{Type: EventSecretStore, Data: SecretStore{Key: SecretSecureHistoryDevice, Value: `{"fake":true}`}})
	emit(Event{Type: EventConversationUpsert, Data: fakeConversations(true, time.Now().UnixMilli())[1]})
	emit(historyEvent("done", "", nil))
	return nil
}

func (f FakeBackend) SkipHistoryRestore(_ context.Context, _ SkipHistoryRestoreArgs, emit func(Event)) error {
	if !f.state().isRestored() {
		emit(historyEvent("skipped", "", nil))
	}
	return nil
}
func (FakeBackend) Logout(context.Context, LogoutArgs, func(Event)) error       { return nil }
func (FakeBackend) Reconnect(context.Context, ReconnectArgs, func(Event)) error { return nil }
func (FakeBackend) ListConversations(context.Context, ListConversationsArgs, func(Event)) error {
	return nil
}

// FetchMessages pages through fakeHistoryLen older messages per conversation, a minute apart and
// ending a few minutes before the fake "now". Bob's E2EE history needs the restore first.
func (f FakeBackend) FetchMessages(ctx context.Context, args FetchMessagesArgs, emit func(Event)) error {
	if !strings.HasPrefix(args.ConversationID, "u:") {
		return &Error{Code: "unknown_conversation", Message: "unknown conversation"}
	}
	st := f.state()
	if args.ConversationID == fakeE2EEConv && !st.isRestored() {
		return setResponseData(ctx, fetchMessagesResult{Cursor: encodeCursor(struct{}{}), NeedsRestore: true})
	}
	var cur struct {
		Offset int `json:"o"`
	}
	if err := decodeCursor(args.Cursor, &cur); err != nil {
		return err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultFetchLimit
	}
	limit = min(limit, maxFetchLimit)
	peer := args.ConversationID[2:]
	end := min(cur.Offset+limit, fakeHistoryLen)
	for k := max(cur.Offset, 0); k < end; k++ {
		text := fmt.Sprintf("Older message %d", k+1)
		m := Message{ID: fmt.Sprintf("fake-%s-old-%d", peer, k), ConversationID: args.ConversationID, ThreadID: args.ConversationID,
			SenderID: peer, TS: st.base - int64(k+3)*60_000, Text: &text, Attachments: []Attachment{}}
		if k%2 == 1 {
			m.SenderID, m.IsOutgoing = "0", true
		}
		if args.ConversationID == fakeE2EEConv {
			m.ThreadID, m.E2EE = "2@msgr", true
		}
		emit(Event{Type: EventMessageUpsert, Data: m})
	}
	cur.Offset = max(end, cur.Offset)
	return setResponseData(ctx, fetchMessagesResult{HasMore: cur.Offset < fakeHistoryLen, Cursor: encodeCursor(cur)})
}

// FetchConversations serves fakeConvPages pages of fakeConvPageSize extra conversations.
func (FakeBackend) FetchConversations(ctx context.Context, args FetchConversationsArgs, emit func(Event)) error {
	var cur struct {
		Page int `json:"p"`
	}
	if err := decodeCursor(args.Cursor, &cur); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if cur.Page >= 0 && cur.Page < fakeConvPages {
		for i := 0; i < fakeConvPageSize; i++ {
			n := 100 + cur.Page*fakeConvPageSize + i + 1
			preview := "Hi"
			emit(Event{Type: EventConversationUpsert, Data: Conversation{ID: fmt.Sprintf("u:%d", n), PeerID: fmt.Sprint(n),
				Name: fmt.Sprintf("Contact %d", n), LastMessagePreview: &preview, LastActivityTS: now - int64(n)*3_600_000, HistoryRestored: true}})
		}
		cur.Page++
	}
	return setResponseData(ctx, struct {
		HasMore bool   `json:"has_more"`
		Cursor  string `json:"cursor"`
	}{cur.Page < fakeConvPages, encodeCursor(cur)})
}

func (FakeBackend) SendText(ctx context.Context, a SendTextArgs, emit func(Event)) error {
	fakeSend(ctx, a.ClientID, emit)
	return nil
}
func (FakeBackend) SendMedia(ctx context.Context, a SendMediaArgs, emit func(Event)) error {
	fakeSend(ctx, a.ClientID, emit)
	return nil
}

// fakeSend reports after the response, like MetaBackend.
func fakeSend(ctx context.Context, id string, emit func(Event)) {
	messageID := "fake-" + id
	result := func() {
		emit(Event{Type: EventMessageSendResult, Data: MessageSendResult{ClientID: id, Status: "sent", MessageID: &messageID}})
	}
	responded := responseSent(ctx)
	if responded == nil {
		result()
		return
	}
	go func() { <-responded; result() }()
}
func (FakeBackend) MarkRead(context.Context, MarkReadArgs, func(Event)) error           { return nil }
func (FakeBackend) DownloadMedia(context.Context, DownloadMediaArgs, func(Event)) error { return nil }
