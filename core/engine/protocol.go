package engine

import "encoding/json"

const (
	CmdLogin                 = "login"
	CmdRestoreHistory        = "restore_history"
	CmdSkipHistoryRestore    = "skip_history_restore"
	CmdLogout                = "logout"
	CmdReconnect             = "reconnect"
	CmdListConversations     = "list_conversations"
	CmdFetchMessages         = "fetch_messages"
	CmdSendText              = "send_text"
	CmdSendMedia             = "send_media"
	CmdMarkRead              = "mark_read"
	CmdDownloadMedia         = "download_media"
	EventCookiesUpdated      = "cookies_updated"
	EventResponse            = "response"
	EventConnectionState     = "connection_state"
	EventHistoryRestoreState = "history_restore_state"
	EventConversationUpsert  = "conversation_upsert"
	EventMessageUpsert       = "message_upsert"
	EventMessageSendResult   = "message_send_result"
	EventReadReceipt         = "read_receipt"
	EventError               = "error"
)

type Request struct {
	RequestID int64           `json:"request_id"`
	Cmd       string          `json:"cmd"`
	Args      json.RawMessage `json:"args"`
}
type Event struct {
	Type      string `json:"type"`
	RequestID *int64 `json:"request_id,omitempty"`
	Data      any    `json:"data"`
}
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

type Response struct {
	OK    bool            `json:"ok"`
	Error *Error          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}
type ConnectionState struct {
	State  string  `json:"state"`
	Reason *string `json:"reason,omitempty"`
}
type HistoryRestoreState struct {
	State  string  `json:"state"`
	Reason *string `json:"reason,omitempty"`
}
type MessageSendResult struct {
	ClientID  string  `json:"client_id"`
	Status    string  `json:"status"`
	MessageID *string `json:"message_id,omitempty"`
	Error     *Error  `json:"error,omitempty"`
}
type ReadReceipt struct {
	ConversationID string `json:"conversation_id"`
	ReaderID       string `json:"reader_id"`
	UntilTS        int64  `json:"until_ts"`
}
type ErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Conversation struct {
	ID                 string  `json:"id"`
	PeerID             string  `json:"peer_id"`
	Name               string  `json:"name"`
	AvatarURL          *string `json:"avatar_url,omitempty"`
	LastMessagePreview *string `json:"last_message_preview,omitempty"`
	LastActivityTS     int64   `json:"last_activity_ts"`
	UnreadCount        int     `json:"unread_count"`
	HistoryRestored    bool    `json:"history_restored"`
}
type Message struct {
	ID             string       `json:"id"`
	ConversationID string       `json:"conversation_id"`
	ThreadID       string       `json:"thread_id"`
	E2EE           bool         `json:"e2ee"`
	SenderID       string       `json:"sender_id"`
	IsOutgoing     bool         `json:"is_outgoing"`
	TS             int64        `json:"ts"`
	Text           *string      `json:"text,omitempty"`
	ClientID       *string      `json:"client_id,omitempty"`
	Attachments    []Attachment `json:"attachments"`
	ReplyToID      *string      `json:"reply_to_id,omitempty"`
}
type Attachment struct {
	Ref        string  `json:"ref"`
	Kind       string  `json:"kind"`
	MIME       string  `json:"mime"`
	Filename   *string `json:"filename,omitempty"`
	Size       *int64  `json:"size,omitempty"`
	Width      *int    `json:"width,omitempty"`
	Height     *int    `json:"height,omitempty"`
	PreviewURL *string `json:"preview_url,omitempty"`
}
type Cookie struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Domain  string `json:"domain"`
	Expires int64  `json:"expires,omitempty"`
}
type LoginArgs struct {
	Cookies  []Cookie `json:"cookies"`
	Platform string   `json:"platform"`
}
type RestoreHistoryArgs struct {
	PIN string `json:"pin"`
}
type SkipHistoryRestoreArgs struct{}
type LogoutArgs struct{}
type ReconnectArgs struct{}
type ListConversationsArgs struct {
	Cursor *string `json:"cursor,omitempty"`
}
type FetchMessagesArgs struct {
	ConversationID string `json:"conversation_id"`
	BeforeTS       *int64 `json:"before_ts,omitempty"`
	Limit          int    `json:"limit"`
}
type SendTextArgs struct {
	ConversationID string `json:"conversation_id"`
	Text           string `json:"text"`
	ClientID       string `json:"client_id"`
}
type SendMediaArgs struct {
	ConversationID string `json:"conversation_id"`
	FilePath       string `json:"file_path"`
	MIME           string `json:"mime"`
	ClientID       string `json:"client_id"`
}
type MarkReadArgs struct {
	ConversationID string `json:"conversation_id"`
	UntilTS        int64  `json:"until_ts"`
}
type DownloadMediaArgs struct {
	AttachmentRef string `json:"attachment_ref"`
	DestPath      string `json:"dest_path"`
}

type CookiesUpdated struct {
	Cookies []Cookie `json:"cookies"`
}
