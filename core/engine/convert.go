package engine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	mediaRefFB = "fb"
	mediaRefWA = "wa"
)

// mediaRef is serialized into Attachment.Ref. Swift stores the ref as an opaque string and passes it
// back in download_media, so the ref must carry everything needed to fetch the file after a restart.
type mediaRef struct {
	Kind string `json:"k"`
	// Non-E2EE (Lightspeed) attachments. URLs expire; FBID, message and thread allow a refresh.
	URL       string `json:"u,omitempty"`
	ExpiresMS int64  `json:"x,omitempty"`
	FBID      string `json:"f,omitempty"`
	MessageID string `json:"m,omitempty"`
	ThreadKey int64  `json:"t,omitempty"`
	// TimestampMS is the message timestamp; a refresh re-fetches the thread around it.
	TimestampMS int64 `json:"ts,omitempty"`
	// E2EE attachments, mirroring msgconv.DirectMediaWhatsApp.
	DirectPath string `json:"dp,omitempty"`
	MediaType  string `json:"mt,omitempty"` // image | video | audio | document | sticker
	MediaKey   []byte `json:"mk,omitempty"`
	SHA256     []byte `json:"sh,omitempty"`
	EncSHA256  []byte `json:"es,omitempty"`
	MIME       string `json:"mi,omitempty"`
}

func encodeMediaRef(ref mediaRef) string {
	raw, _ := json.Marshal(ref)
	return ref.Kind + ":" + base64.RawURLEncoding.EncodeToString(raw)
}

// decodeMediaRef parses Attachment.Ref. The ref comes back from Swift and is untrusted:
// download_media checks it with validateMediaRef (media.go) before fetching anything.
func decodeMediaRef(s string) (mediaRef, error) {
	kind, payload, ok := strings.Cut(s, ":")
	if !ok || (kind != mediaRefFB && kind != mediaRefWA) {
		return mediaRef{}, errors.New("invalid media ref")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return mediaRef{}, err
	}
	var ref mediaRef
	if err = json.Unmarshal(raw, &ref); err != nil {
		return mediaRef{}, err
	}
	if ref.Kind != kind {
		return mediaRef{}, errors.New("invalid media ref")
	}
	return ref, nil
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optInt(v int64) *int {
	if v <= 0 {
		return nil
	}
	i := int(v)
	return &i
}

func optSize(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

func kindFromMIME(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	default:
		return "file"
	}
}

func lsKind(t table.AttachmentType, mime string) string {
	switch t {
	case table.AttachmentTypeImage, table.AttachmentTypeAnimatedImage, table.AttachmentTypeEphemeralImage,
		table.AttachmentTypeSticker, table.AttachmentTypeSelfieSticker, table.AttachmentTypeThirdPartySticker:
		return "image"
	case table.AttachmentTypeVideo, table.AttachmentTypeEphemeralVideo:
		return "video"
	case table.AttachmentTypeAudio, table.AttachmentTypeSoundBite:
		return "audio"
	case table.AttachmentTypeFile:
		return "file"
	}
	return kindFromMIME(mime)
}

// lsAttachment follows msgconv.blobAttachmentToMatrix (pkg/msgconv/from-meta.go:327): the playable URL
// is the full file, the preview URL is used when there is no playable URL.
func lsAttachment(fbid, messageID string, threadKey, ts int64, attType table.AttachmentType, filename string, size int64,
	playable, playableMIME string, playableExp int64, preview, previewMIME string, previewExp int64, mime string, w, h int64) Attachment {
	url, urlMIME, exp := playable, playableMIME, playableExp
	if url == "" {
		url, urlMIME, exp = preview, previewMIME, previewExp
	}
	if urlMIME == "" {
		urlMIME = mime
	}
	if mime == "" {
		mime = urlMIME
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return Attachment{
		Ref: encodeMediaRef(mediaRef{Kind: mediaRefFB, URL: url, ExpiresMS: exp, FBID: fbid, MessageID: messageID,
			ThreadKey: threadKey, TimestampMS: ts, MIME: urlMIME}),
		Kind:       lsKind(attType, mime),
		MIME:       mime,
		Filename:   optString(filename),
		Size:       optSize(size),
		Width:      optInt(w),
		Height:     optInt(h),
		PreviewURL: optString(preview),
	}
}

// fromLS converts a Lightspeed message. The plan names *table.LSUpsertMessage, but attachments arrive in
// separate rows that table.LSTable.WrapMessages joins, so the wrapped form is taken instead.
// ok is false for admin/system messages and for messages with neither text nor a supported
// attachment (XMA shares, polls, unsent messages).
func fromLS(msg *table.WrappedMessage, convID string, selfID int64, clientID *string) (Message, bool) {
	if msg.IsAdminMessage {
		return Message{}, false
	}
	m := Message{
		ID:             msg.MessageId,
		ConversationID: convID,
		ThreadID:       strconv.FormatInt(msg.ThreadKey, 10),
		SenderID:       strconv.FormatInt(msg.SenderId, 10),
		IsOutgoing:     msg.SenderId == selfID,
		TS:             msg.TimestampMs,
		Text:           optString(msg.Text),
		ClientID:       clientID,
		Attachments:    []Attachment{},
	}
	// pkg/msgconv/from-meta.go:250 uses ReplySourceId as the reply target.
	if msg.ReplySourceId != "" {
		m.ReplyToID = &msg.ReplySourceId
	}
	for _, a := range msg.BlobAttachments {
		m.Attachments = append(m.Attachments, lsAttachment(a.AttachmentFbid, msg.MessageId, msg.ThreadKey, msg.TimestampMs, a.AttachmentType,
			a.Filename, a.Filesize, a.PlayableUrl, a.PlayableUrlMimeType, a.PlayableUrlExpirationTimestampMs,
			a.PreviewUrl, a.PreviewUrlMimeType, a.PreviewUrlExpirationTimestampMs, a.AttachmentMimeType, a.PreviewWidth, a.PreviewHeight))
	}
	for _, a := range msg.Attachments {
		m.Attachments = append(m.Attachments, lsAttachment(a.AttachmentFbid, msg.MessageId, msg.ThreadKey, msg.TimestampMs, a.AttachmentType,
			a.Filename, a.Filesize, a.PlayableUrl, a.PlayableUrlMimeType, a.PlayableUrlExpirationTimestampMs,
			a.PreviewUrl, a.PreviewUrlMimeType, a.PreviewUrlExpirationTimestampMs, a.AttachmentMimeType, a.PreviewWidth, a.PreviewHeight))
	}
	for _, a := range msg.Stickers {
		m.Attachments = append(m.Attachments, lsAttachment(a.AttachmentFbid, msg.MessageId, msg.ThreadKey, msg.TimestampMs, table.AttachmentTypeSticker,
			"", 0, a.PlayableUrl, a.PlayableUrlMimeType, a.PlayableUrlExpirationTimestampMs,
			a.PreviewUrl, a.PreviewUrlMimeType, a.PreviewUrlExpirationTimestampMs, "", a.PreviewWidth, a.PreviewHeight))
	}
	if m.Text == nil && len(m.Attachments) == 0 {
		return Message{}, false
	}
	return m, true
}

type waMedia struct {
	transport         *waMediaTransport.WAMediaTransport
	mediaType, kind   string
	defaultMIME, name string
	w, h              uint32
}

// waMediaOf mirrors msgconv.convertWhatsAppMedia (pkg/msgconv/from-whatsapp.go:407) without downloading.
func waMediaOf(content *waConsumerApplication.ConsumerApplication_Content) (*waMedia, *string, error) {
	switch c := content.GetContent().(type) {
	case *waConsumerApplication.ConsumerApplication_Content_ImageMessage:
		t, err := c.ImageMessage.Decode()
		return &waMedia{transport: t.GetIntegral().GetTransport(), mediaType: "image", kind: "image", defaultMIME: "image/jpeg",
			w: t.GetAncillary().GetWidth(), h: t.GetAncillary().GetHeight()}, optString(c.ImageMessage.GetCaption().GetText()), err
	case *waConsumerApplication.ConsumerApplication_Content_StickerMessage:
		t, err := c.StickerMessage.Decode()
		return &waMedia{transport: t.GetIntegral().GetTransport(), mediaType: "sticker", kind: "image", defaultMIME: "image/webp",
			w: t.GetAncillary().GetWidth(), h: t.GetAncillary().GetHeight()}, nil, err
	case *waConsumerApplication.ConsumerApplication_Content_VideoMessage:
		t, err := c.VideoMessage.Decode()
		return &waMedia{transport: t.GetIntegral().GetTransport(), mediaType: "video", kind: "video", defaultMIME: "video/mp4",
			w: t.GetAncillary().GetWidth(), h: t.GetAncillary().GetHeight()}, optString(c.VideoMessage.GetCaption().GetText()), err
	case *waConsumerApplication.ConsumerApplication_Content_AudioMessage:
		t, err := c.AudioMessage.Decode()
		return &waMedia{transport: t.GetIntegral().GetTransport(), mediaType: "audio", kind: "audio", defaultMIME: "audio/ogg"}, nil, err
	case *waConsumerApplication.ConsumerApplication_Content_DocumentMessage:
		t, err := c.DocumentMessage.Decode()
		return &waMedia{transport: t.GetIntegral().GetTransport(), mediaType: "document", kind: "file",
			defaultMIME: "application/octet-stream", name: c.DocumentMessage.GetFileName()}, nil, err
	case *waConsumerApplication.ConsumerApplication_Content_ViewOnceMessage:
		switch v := c.ViewOnceMessage.GetViewOnceContent().(type) {
		case *waConsumerApplication.ConsumerApplication_ViewOnceMessage_ImageMessage:
			return waMediaOf(&waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: v.ImageMessage}})
		case *waConsumerApplication.ConsumerApplication_ViewOnceMessage_VideoMessage:
			return waMediaOf(&waConsumerApplication.ConsumerApplication_Content{Content: &waConsumerApplication.ConsumerApplication_Content_VideoMessage{VideoMessage: v.VideoMessage}})
		}
	}
	return nil, nil, nil
}

// fromWA converts an E2EE consumer message. ok is false for events without displayable consumer content
// (armadillo metadata sync, receipts in disguise, empty payloads).
func fromWA(evt *events.FBMessage, convID string, selfID int64, clientID *string) (Message, bool) {
	content := evt.GetConsumerApplication().GetPayload().GetContent()
	if content == nil || content.GetContent() == nil {
		return Message{}, false
	}
	m := Message{
		ID:             evt.Info.ID,
		ConversationID: convID,
		ThreadID:       evt.Info.Chat.ToNonAD().String(),
		E2EE:           true,
		SenderID:       evt.Info.Sender.User,
		IsOutgoing:     evt.Info.Sender.User == strconv.FormatInt(selfID, 10),
		TS:             evt.Info.Timestamp.UnixMilli(),
		ClientID:       clientID,
		Attachments:    []Attachment{},
	}
	if text := content.GetMessageText().GetText(); text != "" {
		m.Text = &text
	} else if text = content.GetExtendedTextMessage().GetText().GetText(); text != "" {
		m.Text = &text
	}
	media, caption, err := waMediaOf(content)
	if media != nil && err == nil {
		integral, ancillary := media.transport.GetIntegral(), media.transport.GetAncillary()
		mime := ancillary.GetMimetype()
		if mime == "" {
			mime = media.defaultMIME
		}
		m.Attachments = append(m.Attachments, Attachment{
			Ref: encodeMediaRef(mediaRef{Kind: mediaRefWA, DirectPath: integral.GetDirectPath(), MediaType: media.mediaType,
				MediaKey: integral.GetMediaKey(), SHA256: integral.GetFileSHA256(), EncSHA256: integral.GetFileEncSHA256(), MIME: mime}),
			Kind:     media.kind,
			MIME:     mime,
			Filename: optString(media.name),
			Size:     optSize(int64(ancillary.GetFileLength())),
			Width:    optInt(int64(media.w)),
			Height:   optInt(int64(media.h)),
		})
		if m.Text == nil {
			m.Text = caption
		}
	}
	// Reactions, edits, locations and contacts have no Message representation yet.
	if m.Text == nil && len(m.Attachments) == 0 {
		return Message{}, false
	}
	// pkg/msgconv/from-whatsapp.go:895 takes the reply target from the FB application metadata.
	if id := evt.FBApplication.GetMetadata().GetQuotedMessage().GetStanzaID(); id != "" {
		m.ReplyToID = &id
	}
	return m, true
}
