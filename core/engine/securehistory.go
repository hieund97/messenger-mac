package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/securehistory"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waArmadilloBackupMessage"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/keys"
	"google.golang.org/protobuf/proto"
)

const (
	restoreTimeout               = 2 * time.Minute
	historyUploadTimeout         = 2 * time.Minute // history_upload.go
	historyBackupRecheckInterval = time.Hour       // history_upload.go
	e2eeIdentityWait             = time.Minute     // login_history.go waits ConnectWaitTimeout for E2EE
)

// historyPIN is login_history.go historyPINPattern.
var historyPIN = regexp.MustCompile(`^[0-9]{6}$`)

// historyOps is the secure history (encrypted backup) API. Tests set MetaBackend.historyOverride.
type historyOps interface {
	GetBackup(ctx context.Context) (*securehistory.Backup, error)
	// Enroll recovers the backup with the PIN and adds this client as a history device
	// (login_history.go restoreHistory).
	Enroll(ctx context.Context, backup *securehistory.Backup, pin []byte) (*securehistory.Device, error)
	// FetchRange returns the page of peer's thread before ts, newest first.
	FetchRange(ctx context.Context, device *securehistory.Device, peer string, ts int64, count int) (*securehistory.MessageRange, error)
	// UploadSent adds a sent message to the backup; the identity key is filled in by the implementation.
	UploadSent(ctx context.Context, msg *securehistory.MinosSentMessage) error
}

type realHistory struct {
	c    *securehistory.Client
	self int64
	e2ee *e2eeConn
}

func (r realHistory) GetBackup(ctx context.Context) (*securehistory.Backup, error) {
	return r.c.GetBackup(ctx)
}

func (r realHistory) Enroll(ctx context.Context, backup *securehistory.Backup, pin []byte) (*securehistory.Device, error) {
	identity, err := r.e2ee.identity(ctx)
	if err != nil {
		return nil, err
	}
	recovery, err := r.c.RecoverPIN(ctx, backup, pin)
	if err != nil {
		return nil, err
	}
	virtual, err := r.c.GetVirtualDevice(ctx, backup, r.self, recovery.Code)
	if err != nil {
		return nil, err
	}
	epochs, err := virtual.DeriveEpochs()
	if err != nil {
		return nil, err
	}
	enrollment, err := virtual.PrepareDevice(backup.ID, r.c.RegistrationID, identity, epochs)
	if err != nil {
		return nil, err
	}
	// mautrix saves the unconfirmed enrollment first so a crash leaves a record; here the device only
	// reaches Swift (secret_store) once the server accepted it.
	addCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	return r.c.AddDevice(addCtx, enrollment)
}

func (r realHistory) FetchRange(ctx context.Context, device *securehistory.Device, peer string, ts int64, count int) (*securehistory.MessageRange, error) {
	page, err := r.c.FetchMessages(ctx, device, peer, true, ts, count)
	if err != nil {
		return nil, err
	}
	if page.Range.HasMoreBefore != nil && *page.Range.HasMoreBefore && page.Range.Before >= ts {
		return nil, errors.New("history cursor did not move backwards")
	}
	return page, nil
}

func (r realHistory) UploadSent(ctx context.Context, msg *securehistory.MinosSentMessage) error {
	return r.c.UploadSentMessage(ctx, msg)
}

// identity waits for the registered E2EE device and returns its identity key; PrepareDevice signs
// with it (login_history.go waits for the E2EE connection first).
func (e *e2eeConn) identity(ctx context.Context) (*keys.KeyPair, error) {
	ctx, cancel := context.WithTimeout(ctx, e2eeIdentityWait)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if k := e.currentIdentity(); k != nil {
			return k, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("E2EE device is not ready")
		case <-ticker.C:
		}
	}
}

func (e *e2eeConn) currentIdentity() *keys.KeyPair {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.device == nil || e.device.IdentityKey == nil || e.device.IdentityKey.Priv == nil {
		return nil
	}
	return e.device.IdentityKey
}

// historyOps returns the secure history API of the live session.
func (b *MetaBackend) historyOps() (historyOps, error) {
	if b.historyOverride != nil {
		return b.historyOverride, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rc, ok := b.client.(*realMetaClient)
	if !ok || b.index == nil {
		return nil, &Error{Code: "disconnected", Message: "no active session"}
	}
	if b.e2ee == nil {
		return nil, &Error{Code: "e2ee_unavailable", Message: "encrypted chats are not available"}
	}
	return realHistory{c: rc.GetSecureHistoryClient(), self: b.index.self, e2ee: b.e2ee}, nil
}

// historyState is the secure history of one login. The PIN is never stored: only the enrolled device,
// which Swift keeps in the Keychain (secret_store) and hands back at login.
type historyState struct {
	lookup    sync.Mutex
	mu        sync.Mutex
	gen       int // bumped by reset, so a restore that outlived its login is dropped
	dev       *securehistory.Device
	announced bool
	restoring bool
	backupID  string
	checkedAt time.Time
}

func (h *historyState) reset(dev *securehistory.Device) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	h.dev, h.announced, h.restoring, h.backupID, h.checkedAt = dev, false, false, "", time.Time{}
}

func (h *historyState) device() *securehistory.Device {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dev
}

func (h *historyState) setDevice(dev *securehistory.Device) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dev = dev
}

// announce returns the state to report once per login after connecting: done with a device, needed
// without one. ok is false after the first call.
func (h *historyState) announce() (state string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.announced {
		return "", false
	}
	h.announced = true
	if h.dev != nil {
		return "done", true
	}
	return "needed", true
}

func (h *historyState) startRestore() (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restoring {
		return 0, false
	}
	h.restoring = true
	return h.gen, true
}

func (h *historyState) endRestore(gen int, dev *securehistory.Device) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != gen {
		return false
	}
	h.restoring = false
	if dev != nil {
		h.dev = dev
	}
	return true
}

// The account envelope prevents a saved device from being used by another login.
type historySecret struct {
	UserID string                `json:"user_id"`
	Device *securehistory.Device `json:"device"`
}

func deviceFromSecrets(secrets map[string]string, userID int64) *securehistory.Device {
	var saved historySecret
	if json.Unmarshal([]byte(secrets[SecretSecureHistoryDevice]), &saved) != nil || saved.UserID != strconv.FormatInt(userID, 10) || saved.Device == nil {
		return nil
	}
	d := saved.Device
	if d.ID == "" || d.BackupID == "" || len(d.MailboxRootKey) != 32 || len(d.StoragePublicKey) != 32 || len(d.StoragePrivateKey) != 32 || len(d.OCMFClientState) == 0 || len(d.Epochs) == 0 {
		return nil
	}
	for id, e := range d.Epochs {
		if id == "" || e.ID != id || len(e.RootKey) != 32 || len(e.AnonID) == 0 || len(e.AnonID) > 11 {
			return nil
		}
		if _, err := strconv.ParseUint(string(e.AnonID), 10, 64); err != nil {
			return nil
		}
	}
	return d
}
func deviceSecret(dev *securehistory.Device, userID int64) (Event, error) {
	raw, err := json.Marshal(historySecret{UserID: strconv.FormatInt(userID, 10), Device: dev})
	if err != nil {
		return Event{}, err
	}
	return Event{Type: EventSecretStore, Data: SecretStore{Key: SecretSecureHistoryDevice, Value: string(raw)}}, nil
}

// Upstream currently exposes these failures as text, not sentinel errors.
func invalidHistoryDevice(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "archive is no longer available") || strings.Contains(msg, "different backup") || strings.Contains(msg, "not enrolled") || (strings.Contains(msg, "epoch") && strings.Contains(msg, "mismatch"))
}
func (b *MetaBackend) invalidateHistory(s liveSession, dev *securehistory.Device) {
	b.history.mu.Lock()
	if b.history.dev != dev {
		b.history.mu.Unlock()
		return
	}
	b.history.gen++
	b.history.dev, b.history.announced, b.history.restoring, b.history.backupID, b.history.checkedAt = nil, true, false, "", time.Time{}
	b.history.mu.Unlock()
	s.index.SetHistoryRestored(false, b.loginEmit())
	emit := b.loginEmit()
	emit(Event{Type: EventSecretStore, Data: SecretStore{Key: SecretSecureHistoryDevice, Value: ""}})
	emit(historyEvent("needed", "", nil))
}

func historyEvent(state, reason string, attempts *int) Event {
	s := HistoryRestoreState{State: state, AttemptsRemaining: attempts}
	if reason != "" {
		s.Reason = &reason
	}
	return Event{Type: EventHistoryRestoreState, Data: s}
}

// announceHistory reports history_restore_state once per login, after the first connect.
func (b *MetaBackend) announceHistory(client metaClient) {
	emit := b.sink(client)
	if emit == nil {
		return
	}
	if state, ok := b.history.announce(); ok {
		emit(historyEvent(state, "", nil))
	}
}

// restoreFailure maps a restore error to the reason Swift shows (login_history.go SubmitUserInput).
func restoreFailure(err error) (string, *int) {
	var recovery *securehistory.RecoveryError
	switch {
	case errors.Is(err, securehistory.ErrNoPINBackup):
		return "no_backup", nil
	case errors.Is(err, types.ErrPleaseLogIn), errors.Is(err, httpclient.ErrTokenInvalidated):
		return "auth_required", nil
	case errors.As(err, &recovery):
		if recovery.AttemptsRemaining != nil && *recovery.AttemptsRemaining == 0 {
			return "locked", recovery.AttemptsRemaining
		}
		return "wrong_pin", recovery.AttemptsRemaining
	default:
		return "other", nil
	}
}

// RestoreHistory recovers the secure history with the PIN: in_progress, then done or failed. The
// response comes after the final state; failures are reported through the state, not the response.
// On success the enrolled device goes to Swift as secret_store and conversations with an E2EE thread
// get history_restored=true; their history is then loaded by fetch_messages (no messages are pushed).
func (b *MetaBackend) RestoreHistory(ctx context.Context, args RestoreHistoryArgs, emit func(Event)) error {
	s, err := b.liveSession()
	if err != nil {
		return err
	}
	if emit == nil {
		emit = func(Event) {}
	}
	if b.history.device() != nil {
		emit(historyEvent("done", "", nil))
		return nil
	}
	if !historyPIN.MatchString(args.PIN) {
		emit(historyEvent("failed", "wrong_pin", nil))
		return nil
	}
	gen, ok := b.history.startRestore()
	if !ok {
		return &Error{Code: "busy", Message: "history restore already in progress"}
	}
	var dev *securehistory.Device
	ended := false
	defer func() {
		if !ended {
			b.history.endRestore(gen, nil)
		}
	}()
	emit(historyEvent("in_progress", "", nil))
	ops, err := b.historyOps()
	if err != nil {
		emit(historyEvent("failed", "other", nil))
		return nil
	}
	rctx, cancel := withSession(ctx, s.ctx)
	defer cancel()
	rctx, cancelTimeout := context.WithTimeout(rctx, restoreTimeout)
	defer cancelTimeout()
	pin := []byte(args.PIN)
	defer clear(pin)
	backup, err := ops.GetBackup(rctx)
	if err == nil {
		dev, err = ops.Enroll(rctx, backup, pin)
	}
	if err == nil && (dev == nil || dev.ID == "") {
		err = errors.New("enrollment returned no device")
	}
	if err != nil {
		dev = nil
		reason, attempts := restoreFailure(err)
		logger := s.index.logger()
		// Errors from securehistory never contain the PIN; the log writer redacts URL queries.
		logger.Error().Err(err).Str("reason", reason).Msg("Secure history restore failed")
		emit(historyEvent("failed", reason, attempts))
		return nil
	}
	secret, err := deviceSecret(dev, s.index.self)
	if err != nil {
		dev = nil
		emit(historyEvent("failed", "other", nil))
		return nil
	}
	if !b.history.endRestore(gen, dev) {
		return &Error{Code: "disconnected", Message: "the session ended during the restore"}
	}
	ended = true
	emit(secret)
	s.index.SetHistoryRestored(true, b.loginEmit())
	emit(historyEvent("done", "", nil))
	return nil
}

// SkipHistoryRestore leaves the E2EE history unrestored for this login; restore_history still works
// later. It does nothing once the history is restored.
func (b *MetaBackend) SkipHistoryRestore(_ context.Context, _ SkipHistoryRestoreArgs, emit func(Event)) error {
	if _, err := b.liveSession(); err != nil {
		return err
	}
	if b.history.device() != nil || emit == nil {
		return nil
	}
	emit(historyEvent("skipped", "", nil))
	return nil
}

// applyHistoryEpochs adds the epochs a page delivered to the device and hands the updated device to
// Swift (history.go saveHistoryEpochs persists it the same way).
func (b *MetaBackend) applyHistoryEpochs(dev *securehistory.Device, set *securehistory.EpochDerivationSet) (*securehistory.Device, error) {
	b.history.mu.Lock()
	current := b.history.dev
	if current == nil || current.ID != dev.ID || current.BackupID != dev.BackupID {
		b.history.mu.Unlock()
		return nil, errors.New("history enrollment changed while fetching messages")
	}
	if set == nil || len(set.Edges) == 0 {
		b.history.mu.Unlock()
		return current, nil
	}
	next, err := current.WithEpochs(set)
	if err != nil {
		b.history.mu.Unlock()
		return nil, err
	}
	b.history.dev = next
	b.history.mu.Unlock()
	b.mu.Lock()
	index := b.index
	b.mu.Unlock()
	if index != nil {
		if secret, err := deviceSecret(next, index.self); err == nil {
			b.loginEmit()(secret)
		}
	}
	return next, nil
}

// archiveMessage converts a decrypted secure history entry (backfill_e2ee.go convertArchiveMessage):
// the latest edit replaces the text, reactions and other supplementals are dropped.
func archiveMessage(dm *securehistory.DecryptedMessage, chat waTypes.JID, convID string, self int64) (Message, bool) {
	parent := decodeArchive(dm.Message.Message, dm.Message.TimestampMS, chat, dm.ID, self)
	if parent == nil {
		return Message{}, false
	}
	var latest *waCommon.MessageText
	var latestTS int64
	for _, sup := range dm.Supplementals {
		if sup == nil {
			continue
		}
		decoded := decodeArchive(sup.Message, sup.TimestampMS, chat, "", self)
		if decoded == nil || decoded.Info.Sender != parent.Info.Sender {
			continue
		}
		edit := decoded.GetConsumerApplication().GetPayload().GetContent().GetEditMessage()
		if edit == nil || edit.GetMessage() == nil || (edit.GetKey().GetID() != "" && edit.GetKey().GetID() != parent.Info.ID) {
			continue
		}
		if latest == nil || sup.TimestampMS > latestTS {
			latest, latestTS = edit.GetMessage(), sup.TimestampMS
		}
	}
	if latest != nil {
		parent.Message = consumerMessage(&waConsumerApplication.ConsumerApplication_Content{
			Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{MessageText: latest},
		})
	}
	paths := map[string]string{}
	for _, a := range dm.Attachments {
		if a.ObjectID != "" && a.URL != "" {
			paths[a.ObjectID] = a.URL
		}
	}
	return fromWAWithPaths(parent, convID, self, nil, paths)
}

// decodeArchive is backfill_e2ee.go decodeArchiveMessage. id overrides the metadata message ID (the
// archive entry ID of the top-level message); "" keeps the metadata one.
func decodeArchive(msg *waArmadilloBackupMessage.BackupMessage, ts int64, chat waTypes.JID, id string, self int64) *events.FBMessage {
	payload, ok := msg.GetPayload().(*waArmadilloBackupMessage.BackupMessage_EncryptedTransportMessage)
	if !ok {
		return nil // admin messages and transport events have nothing to show
	}
	var app waMsgApplication.MessageApplication
	if proto.Unmarshal(payload.EncryptedTransportMessage, &app) != nil {
		return nil
	}
	decoded, err := whatsmeow.DecodeFBApplication(&app)
	if err != nil {
		return nil
	}
	md := msg.GetMetadata()
	if md.GetMessageID() == "" || md.GetSenderID() == "" || ts < 0 {
		return nil
	}
	if id == "" {
		id = md.GetMessageID()
	}
	decoded.Info = waTypes.MessageInfo{
		MessageSource: waTypes.MessageSource{
			Chat:     chat,
			Sender:   waTypes.NewJID(md.GetSenderID(), waTypes.MessengerServer),
			IsFromMe: md.GetSenderID() == strconv.FormatInt(self, 10),
		},
		ID:        id,
		Timestamp: time.UnixMilli(ts),
	}
	return &decoded
}

// marshalSentFBMessage rebuilds the MessageApplication that SendFBMessage encrypted
// (history_upload.go marshalSentFBMessage); the franking tag is computed over it.
func marshalSentFBMessage(msg *waConsumerApplication.ConsumerApplication, meta *waMsgApplication.MessageApplication_Metadata) ([]byte, error) {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(&waMsgApplication.MessageApplication{
		Payload: &waMsgApplication.MessageApplication_Payload{
			Content: &waMsgApplication.MessageApplication_Payload_SubProtocol{SubProtocol: &waMsgApplication.MessageApplication_SubProtocolPayload{
				FutureProof: waCommon.FutureProofBehavior_PLACEHOLDER.Enum(),
				SubProtocol: &waMsgApplication.MessageApplication_SubProtocolPayload_ConsumerMessage{
					ConsumerMessage: &waCommon.SubProtocol{Payload: payload, Version: proto.Int32(whatsmeow.FBConsumerMessageVersion)},
				},
			}},
		},
		Metadata: meta,
	})
}

// queueHistoryUpload is the default E2EESentHook: it adds a sent E2EE message to the secure history
// backup in the background (history_upload.go uploadSentMessageToHistory). Failures are only logged
// and never change the send result.
func (b *MetaBackend) queueHistoryUpload(ctx context.Context, chat waTypes.JID, id string, msg *waConsumerApplication.ConsumerApplication,
	meta *waMsgApplication.MessageApplication_Metadata, _ time.Time) {
	if chat.Server != waTypes.MessengerServer || msg == nil || meta == nil || len(meta.GetFrankingKey()) == 0 {
		return
	}
	b.mu.Lock()
	ok := b.cookies != nil && b.cookies.Platform.IsMessenger() && b.index != nil
	var index *ThreadIndex
	if ok {
		index = b.index
	}
	b.mu.Unlock()
	if !ok {
		return
	}
	ops, err := b.historyOps()
	if err != nil {
		return
	}
	// Capture identity before the session can be closed while the upload is queued.
	var identity *keys.KeyPair
	if real, ok := ops.(realHistory); ok {
		if real.c.AppID == "" {
			index.logger().Warn().Msg("Skipping history upload without app ID")
			return
		}
		identity = real.e2ee.currentIdentity()
		if identity == nil {
			index.logger().Warn().Msg("Skipping history upload without identity")
			return
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(ctx, historyUploadTimeout)
		defer cancel()
		if err := b.uploadSent(ctx, ops, index.self, chat, id, msg, meta, identity); err != nil {
			logger := index.logger()
			logger.Error().Err(err).Msg("Failed to upload sent message to encrypted history")
		}
	}()
}

func (b *MetaBackend) uploadSent(ctx context.Context, ops historyOps, self int64, chat waTypes.JID, id string,
	msg *waConsumerApplication.ConsumerApplication, meta *waMsgApplication.MessageApplication_Metadata, identity *keys.KeyPair) error {
	backupID, err := b.historyBackupID(ctx, ops)
	if err != nil {
		return fmt.Errorf("get backup ID: %w", err)
	} else if backupID == "" {
		return nil // no backup: nothing to add to
	}
	other, err := strconv.ParseInt(chat.User, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat: %w", err)
	}
	otid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid message ID: %w", err)
	}
	transport, err := marshalSentFBMessage(msg, meta)
	if err != nil {
		return err
	}
	franking := hmac.New(sha256.New, meta.GetFrankingKey())
	franking.Write(transport)
	sent := &securehistory.MinosSentMessage{
		BackupID:       backupID,
		SenderID:       self,
		OtherUserID:    other,
		MessageID:      id,
		TimestampMS:    otid >> 22,
		TransportBytes: transport,
		FrankingTag:    franking.Sum(nil),
	}
	if identity != nil {
		sent.IdentityPriv, sent.IdentityPub = identity.Priv, identity.Pub
	}
	return ops.UploadSent(ctx, sent)
}

// historyBackupID is history_upload.go getHistoryBackupID: the enrolled device's backup, else the
// account's backup looked up at most once per historyBackupRecheckInterval.
func (b *MetaBackend) historyBackupID(ctx context.Context, ops historyOps) (string, error) {
	h := b.history
	h.lookup.Lock() // one lookup at a time; h.mu is not held over the request
	defer h.lookup.Unlock()
	h.mu.Lock()
	gen := h.gen
	if h.dev != nil && h.dev.BackupID != "" {
		defer h.mu.Unlock()
		return h.dev.BackupID, nil
	} else if h.backupID != "" || (!h.checkedAt.IsZero() && time.Since(h.checkedAt) < historyBackupRecheckInterval) {
		defer h.mu.Unlock()
		return h.backupID, nil
	}
	h.mu.Unlock()
	backup, err := ops.GetBackup(ctx)
	id := ""
	if err == nil {
		id = backup.ID
	} else if !errors.Is(err, securehistory.ErrNoPINBackup) {
		return "", err
	}
	h.mu.Lock()
	if h.gen == gen {
		h.backupID, h.checkedAt = id, time.Now()
	}
	h.mu.Unlock()
	return id, nil
}

func (x *ThreadIndex) logger() *zerolog.Logger {
	x.mu.Lock()
	defer x.mu.Unlock()
	l := x.log
	return &l
}
