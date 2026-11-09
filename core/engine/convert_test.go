package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func consumerContent(content waConsumerApplication.ConsumerApplication_Content_Content) *waConsumerApplication.ConsumerApplication {
	return &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_Content{Content: &waConsumerApplication.ConsumerApplication_Content{Content: content}},
	}}
}

func consumerText(text string) *waConsumerApplication.ConsumerApplication {
	return consumerContent(&waConsumerApplication.ConsumerApplication_Content_MessageText{MessageText: &waCommon.MessageText{Text: proto.String(text)}})
}

func TestFromLSImageAttachment(t *testing.T) {
	msg := &table.WrappedMessage{
		LSInsertMessage: &table.LSInsertMessage{ThreadKey: testPeer, MessageId: "mid.$img", OfflineThreadingId: "7123", SenderId: testPeer, TimestampMs: 1234},
		BlobAttachments: []*table.LSInsertBlobAttachment{{
			AttachmentFbid:     "998877",
			AttachmentType:     table.AttachmentTypeImage,
			AttachmentMimeType: "image/jpeg",
			Filename:           "image-998877.jpg",
			Filesize:           2048,
			PreviewUrl:         "https://cdn.example.invalid/p.jpg",
			PreviewUrlMimeType: "image/jpeg",
			PreviewWidth:       640,
			PreviewHeight:      480,
			MessageId:          "mid.$img",
			ThreadKey:          testPeer,
		}},
	}
	clientID := "client-1"
	m, ok := fromLS(msg, "u:100053372173569", testSelf, &clientID)
	if !ok {
		t.Fatal("not converted")
	}
	if m.ID != "mid.$img" || m.ConversationID != "u:100053372173569" || m.ThreadID != "100053372173569" || m.E2EE || m.IsOutgoing {
		t.Fatalf("message = %+v", m)
	}
	if m.SenderID != "100053372173569" || m.TS != 1234 || m.Text != nil || m.ClientID == nil || *m.ClientID != "client-1" {
		t.Fatalf("message = %+v", m)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	a := m.Attachments[0]
	if a.Kind != "image" || a.MIME != "image/jpeg" || a.Width == nil || *a.Width != 640 || a.Height == nil || *a.Height != 480 {
		t.Fatalf("attachment = %+v", a)
	}
	if a.PreviewURL == nil || *a.PreviewURL != "https://cdn.example.invalid/p.jpg" || a.Size == nil || *a.Size != 2048 {
		t.Fatalf("attachment = %+v", a)
	}
	ref, err := decodeMediaRef(a.Ref)
	if err != nil || ref.Kind != mediaRefFB || ref.FBID != "998877" || ref.MessageID != "mid.$img" || ref.URL != "https://cdn.example.invalid/p.jpg" {
		t.Fatalf("ref = %+v, %v", ref, err)
	}

	text, ok := fromLS(&table.WrappedMessage{LSInsertMessage: &table.LSInsertMessage{ThreadKey: testPeer, MessageId: "mid.$t", SenderId: testSelf, Text: "hi"}}, "u:1", testSelf, nil)
	if !ok || text.Attachments == nil || !text.IsOutgoing || text.Text == nil || *text.Text != "hi" || text.ClientID != nil {
		t.Fatalf("text = %+v", text)
	}
	raw, _ := json.Marshal(text)
	if !strings.Contains(string(raw), `"attachments":[]`) {
		t.Fatal(string(raw))
	}
}

func TestFromWAFileAttachment(t *testing.T) {
	doc := &waConsumerApplication.ConsumerApplication_DocumentMessage{FileName: proto.String("báo cáo.pdf")}
	err := doc.Set(&waMediaTransport.DocumentTransport{
		Integral: &waMediaTransport.DocumentTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
			Integral: &waMediaTransport.WAMediaTransport_Integral{
				FileSHA256: []byte{1}, MediaKey: []byte{2}, FileEncSHA256: []byte{3}, DirectPath: proto.String("/v/t62/abc"),
			},
			Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{FileLength: proto.Uint64(4096), Mimetype: proto.String("application/pdf")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	evt := &events.FBMessage{Message: consumerContent(&waConsumerApplication.ConsumerApplication_Content_DocumentMessage{DocumentMessage: doc})}
	evt.Info.ID = "3EB0FILE"
	evt.Info.Chat = waTypes.NewJID("100053372173569", waTypes.MessengerServer)
	evt.Info.Sender = waTypes.JID{User: "1000", Device: 5, Server: waTypes.MessengerServer}
	evt.Info.Timestamp = time.UnixMilli(777000)
	clientID := "client-2"
	m, ok := fromWA(evt, "u:100053372173569", testSelf, &clientID)
	if !ok {
		t.Fatal("not converted")
	}
	if m.ID != "3EB0FILE" || !m.E2EE || !m.IsOutgoing || m.SenderID != "1000" || m.TS != 777000 || m.ThreadID != "100053372173569@msgr" {
		t.Fatalf("message = %+v", m)
	}
	if m.Text != nil || m.ClientID == nil || *m.ClientID != "client-2" || len(m.Attachments) != 1 {
		t.Fatalf("message = %+v", m)
	}
	a := m.Attachments[0]
	if a.Kind != "file" || a.MIME != "application/pdf" || a.Filename == nil || *a.Filename != "báo cáo.pdf" || a.Size == nil || *a.Size != 4096 {
		t.Fatalf("attachment = %+v", a)
	}
	ref, err := decodeMediaRef(a.Ref)
	if err != nil || ref.Kind != mediaRefWA || ref.DirectPath != "/v/t62/abc" || ref.MediaType != "document" || string(ref.MediaKey) != "\x02" {
		t.Fatalf("ref = %+v, %v", ref, err)
	}

	if _, ok := fromWA(&events.FBMessage{}, "u:1", testSelf, nil); ok {
		t.Fatal("empty message converted")
	}
}

func TestUnsupportedMessagesSkipped(t *testing.T) {
	admin := &table.WrappedMessage{LSInsertMessage: &table.LSInsertMessage{ThreadKey: testPeer, MessageId: "mid.$a", SenderId: testPeer, Text: "đã đổi chủ đề", IsAdminMessage: true}}
	if _, ok := fromLS(admin, "u:1", testSelf, nil); ok {
		t.Fatal("admin message converted")
	}
	xma := &table.WrappedMessage{
		LSInsertMessage: &table.LSInsertMessage{ThreadKey: testPeer, MessageId: "mid.$x", SenderId: testPeer},
		XMAAttachments:  []*table.WrappedXMA{{LSInsertXmaAttachment: &table.LSInsertXmaAttachment{AttachmentFbid: "1"}}},
	}
	if _, ok := fromLS(xma, "u:1", testSelf, nil); ok {
		t.Fatal("xma-only message converted")
	}
	location := &events.FBMessage{Message: consumerContent(&waConsumerApplication.ConsumerApplication_Content_LocationMessage{
		LocationMessage: &waConsumerApplication.ConsumerApplication_LocationMessage{},
	})}
	location.Info.ID = "3EB0LOC"
	location.Info.Chat = waTypes.NewJID("100053372173569", waTypes.MessengerServer)
	if _, ok := fromWA(location, "u:1", testSelf, nil); ok {
		t.Fatal("location message converted")
	}

	// Through the index: no message_upsert, and a skipped message never creates a conversation.
	idx := NewThreadIndex(testSelf)
	var log eventLog
	idx.ApplyTable(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{
		{ThreadKey: testPeer, MessageId: "mid.$a", SenderId: testPeer, Text: "x", IsAdminMessage: true},
		{ThreadKey: testPeer, MessageId: "mid.$e", SenderId: testPeer},
	}}, log.emit)
	idx.ApplyWAMessage(location, log.emit)
	if len(log.events) != 0 || len(idx.Conversations()) != 0 {
		t.Fatalf("events = %+v", log.events)
	}
}
