package engine

import "context"

type FakeBackend struct{}

func (FakeBackend) Login(_ context.Context, _ LoginArgs, emit func(Event)) error {
	emit(Event{Type: EventConnectionState, Data: ConnectionState{State: "connected"}})
	emit(Event{Type: EventConversationUpsert, Data: Conversation{ID: "u:1", PeerID: "1", Name: "Alice", LastActivityTS: 3, HistoryRestored: true}})
	emit(Event{Type: EventConversationUpsert, Data: Conversation{ID: "u:2", PeerID: "2", Name: "Bob", LastActivityTS: 2, HistoryRestored: true}})
	for i, msg := range []struct{ id, conv, text string }{{"1", "u:1", "Hello"}, {"2", "u:1", "Hi"}, {"3", "u:2", "Hello"}} {
		text := msg.text
		emit(Event{Type: EventMessageUpsert, Data: Message{ID: msg.id, ConversationID: msg.conv, ThreadID: msg.conv, SenderID: msg.conv[2:], TS: int64(i + 1), Text: &text, Attachments: []Attachment{}}})
	}
	return nil
}
func (FakeBackend) RestoreHistory(context.Context, RestoreHistoryArgs, func(Event)) error { return nil }
func (FakeBackend) SkipHistoryRestore(context.Context, SkipHistoryRestoreArgs, func(Event)) error {
	return nil
}
func (FakeBackend) Logout(context.Context, LogoutArgs, func(Event)) error       { return nil }
func (FakeBackend) Reconnect(context.Context, ReconnectArgs, func(Event)) error { return nil }
func (FakeBackend) ListConversations(context.Context, ListConversationsArgs, func(Event)) error {
	return nil
}
func (FakeBackend) FetchMessages(context.Context, FetchMessagesArgs, func(Event)) error { return nil }
func (FakeBackend) SendText(_ context.Context, a SendTextArgs, emit func(Event)) error {
	fakeSend(a.ClientID, emit)
	return nil
}
func (FakeBackend) SendMedia(_ context.Context, a SendMediaArgs, emit func(Event)) error {
	fakeSend(a.ClientID, emit)
	return nil
}
func fakeSend(id string, emit func(Event)) {
	messageID := "fake-" + id
	emit(Event{Type: EventMessageSendResult, Data: MessageSendResult{ClientID: id, Status: "sent", MessageID: &messageID}})
}
func (FakeBackend) MarkRead(context.Context, MarkReadArgs, func(Event)) error           { return nil }
func (FakeBackend) DownloadMedia(context.Context, DownloadMediaArgs, func(Event)) error { return nil }
