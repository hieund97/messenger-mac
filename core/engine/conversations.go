package engine

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix/graphql"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const dedupeCapacity = 5000

// conversationID is the stable key for a 1-1 conversation. The normal Messenger thread and the E2EE
// thread with the same person share it, so the UI shows a single conversation.
func conversationID(selfID, peerID int64) string {
	_ = selfID // Kept in the signature: a note-to-self conversation is keyed by the own ID like any peer.
	return "u:" + strconv.FormatInt(peerID, 10)
}

func e2eeThreadID(user int64) string {
	return waTypes.NewJID(strconv.FormatInt(user, 10), waTypes.MessengerServer).String()
}

type contactInfo struct{ name, avatar string }

type convEntry struct {
	conv          Conversation
	peer          int64
	normalThread  int64  // Lightspeed thread key, 0 if unknown
	e2eeThread    string // "<peer>@msgr", empty if unknown
	lsE2EEKey     int64  // Lightspeed key of the E2EE thread (hybrid mapping), 0 if unknown
	linked        bool   // seen in LSCreateOpenToE2EEThreadLink
	threadName    string
	threadPicture string
	// Oldest message the core has handled per thread: fetch_messages without a cursor starts there.
	oldestLS msgPos
	oldestWA msgPos
}

// msgPos is a position in a thread, ordered by (ts, id) so messages with the same timestamp are kept apart.
type msgPos struct {
	ts int64
	id string
}

func (p msgPos) before(q msgPos) bool { return p.ts < q.ts || (p.ts == q.ts && p.id < q.id) }

func (p *msgPos) lower(ts int64, id string) {
	if q := (msgPos{ts, id}); p.id == "" || q.before(*p) {
		*p = q
	}
}

// ThreadIndex maps Lightspeed thread keys and E2EE JIDs onto conversations. Normal threads use the
// decimal thread key as thread_id, E2EE threads the JID string, so the two never collide.
type ThreadIndex struct {
	mu      sync.Mutex
	self    int64
	log     zerolog.Logger
	convs   map[string]*convEntry
	threads map[string]string // thread_id -> conversation_id
	hybrid  map[int64]int64   // Lightspeed thread key -> ThreadJID user
	ignored map[int64]bool    // thread keys of group or other non-1-1 threads
	pending map[int64]int64   // E2EE links whose open thread is not known yet
	// confirmed holds hybrid thread keys known to be 1-1; pendingE2EE holds type-15 threads whose
	// JID mapping has not arrived.
	confirmed   map[int64]bool
	pendingE2EE map[int64]lsThread
	contacts    map[int64]contactInfo

	seen     map[string]*list.Element
	seenList *list.List

	// Filled by send (Task 5): OTID -> client_id and E2EE message ID -> client_id, so echoes of messages
	// sent from this app carry the client_id that Swift generated.
	otidClient map[string]string
	waClient   map[string]string
	// waUnread holds recent incoming E2EE message IDs per conversation for mark_read.
	waUnread map[string][]waIncoming
	// historyMuted holds Lightspeed thread keys whose upserted (history) messages are not emitted until
	// the given time: a media URL refresh re-fetches a page of old messages that Swift did not ask for.
	historyMuted map[int64]time.Time
	// collectors receive the upserted history of thread keys a fetch_messages is paging through.
	collectors map[int64]*lsCollector
	// historyRestored is true while a secure history device is enrolled; it sets history_restored of
	// conversations with an E2EE thread.
	historyRestored bool
}

// lsCollector takes the next history batch of one thread (backfill.go BackfillCollector, one batch).
type lsCollector struct {
	anchorTS int64
	ch       chan *table.UpsertMessages
}

type waIncoming struct {
	id string
	ts int64
}

// waUnreadCapacity bounds waUnread per conversation; older IDs are dropped first.
const waUnreadCapacity = 200

func NewThreadIndex(selfID int64) *ThreadIndex {
	return &ThreadIndex{
		self:         selfID,
		log:          zerolog.Nop(),
		convs:        map[string]*convEntry{},
		threads:      map[string]string{},
		hybrid:       map[int64]int64{},
		ignored:      map[int64]bool{},
		pending:      map[int64]int64{},
		confirmed:    map[int64]bool{},
		pendingE2EE:  map[int64]lsThread{},
		contacts:     map[int64]contactInfo{},
		seen:         map[string]*list.Element{},
		seenList:     list.New(),
		otidClient:   map[string]string{},
		waClient:     map[string]string{},
		waUnread:     map[string][]waIncoming{},
		historyMuted: map[int64]time.Time{},
		collectors:   map[int64]*lsCollector{},
	}
}

// muteHistory stops upserted messages of thread key from being emitted until the given time. Messages
// already emitted are deduplicated anyway, so only the media URLs of the re-fetched page are used.
func (x *ThreadIndex) muteHistory(key int64, until time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := time.Now()
	for k, t := range x.historyMuted {
		if now.After(t) {
			delete(x.historyMuted, k)
		}
	}
	if until.After(x.historyMuted[key]) {
		x.historyMuted[key] = until
	}
}

func (x *ThreadIndex) historyIsMuted(key int64) bool {
	t, ok := x.historyMuted[key]
	return ok && time.Now().Before(t)
}

// RememberClientID records the identifiers a send used, so the echo can be matched back.
func (x *ThreadIndex) RememberClientID(otid, waMessageID, clientID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if otid != "" {
		x.otidClient[otid] = clientID
	}
	if waMessageID != "" {
		x.waClient[waMessageID] = clientID
	}
}

// SendRoute returns the thread a message to convID should go to. An E2EE thread wins as soon as one is
// known (cutover link, type-15 thread, hybrid mapping or an incoming E2EE message): once Messenger has
// moved a chat to E2EE, the official clients only write there.
func (x *ThreadIndex) SendRoute(convID string) (threadID string, e2ee bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.convs[convID]
	switch {
	case e == nil:
		return "", false
	case e.e2eeThread != "":
		return e.e2eeThread, true
	case e.normalThread != 0:
		return strconv.FormatInt(e.normalThread, 10), false
	}
	return "", false
}

// Conversations returns all known conversations, most recent first.
func (x *ThreadIndex) Conversations() []Conversation {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]Conversation, 0, len(x.convs))
	for _, e := range x.convs {
		out = append(out, e.conv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActivityTS != out[j].LastActivityTS {
			return out[i].LastActivityTS > out[j].LastActivityTS
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (x *ThreadIndex) markSeen(id string) bool {
	if _, ok := x.seen[id]; ok {
		return false
	}
	x.seen[id] = x.seenList.PushBack(id)
	if x.seenList.Len() > dedupeCapacity {
		oldest := x.seenList.Front()
		x.seenList.Remove(oldest)
		delete(x.seen, oldest.Value.(string))
	}
	return true
}

func (x *ThreadIndex) displayName(e *convEntry) string {
	if c, ok := x.contacts[e.peer]; ok && c.name != "" {
		return c.name
	}
	if e.threadName != "" {
		return e.threadName
	}
	return strconv.FormatInt(e.peer, 10)
}

func (x *ThreadIndex) refresh(e *convEntry) {
	e.conv.Name = x.displayName(e)
	avatar := e.threadPicture
	if c, ok := x.contacts[e.peer]; ok && c.avatar != "" {
		avatar = c.avatar
	}
	e.conv.AvatarURL = optString(avatar)
	// Normal threads always have their history; an E2EE thread needs the secure history restored.
	e.conv.HistoryRestored = e.e2eeThread == "" || x.historyRestored
}

// SetHistoryRestored updates history_restored of every conversation with an E2EE thread.
func (x *ThreadIndex) SetHistoryRestored(restored bool, emit func(Event)) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.historyRestored == restored {
		return
	}
	x.historyRestored = restored
	if emit == nil {
		emit = func(Event) {}
	}
	for _, e := range x.convs {
		if e.e2eeThread != "" {
			x.update(e.peer, emit, nil)
		}
	}
}

// addCollector registers the collector of the next history batch of thread key. Only one fetch pages a
// thread at a time.
func (x *ThreadIndex) addCollector(key, anchorTS int64) (*lsCollector, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, busy := x.collectors[key]; busy {
		return nil, false
	}
	c := &lsCollector{anchorTS: anchorTS, ch: make(chan *table.UpsertMessages, 1)}
	x.collectors[key] = c
	return c, true
}

func (x *ThreadIndex) removeCollector(key int64, c *lsCollector) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.collectors[key] == c {
		delete(x.collectors, key)
	}
}

// collect hands an upsert to the collector of its thread. Like handleUpsertMessages it ignores ranges
// newer than the anchor: a late answer to an earlier batch. Called with x.mu held.
func (x *ThreadIndex) collect(key int64, upsert *table.UpsertMessages) bool {
	c := x.collectors[key]
	if c == nil || upsert.Range == nil || (c.anchorTS > 0 && upsert.Range.MaxTimestampMsTemplate > c.anchorTS) {
		return false
	}
	delete(x.collectors, key)
	c.ch <- upsert
	return true
}

func ptrEqual[T comparable](a, b *T) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameConversation(a, b Conversation) bool {
	return a.ID == b.ID && a.PeerID == b.PeerID && a.Name == b.Name && ptrEqual(a.AvatarURL, b.AvatarURL) &&
		ptrEqual(a.LastMessagePreview, b.LastMessagePreview) && a.LastActivityTS == b.LastActivityTS &&
		a.UnreadCount == b.UnreadCount && a.HistoryRestored == b.HistoryRestored
}

// update runs fn on the conversation of peer (creating it when needed) and emits conversation_upsert
// when anything visible changed.
func (x *ThreadIndex) update(peer int64, emit func(Event), fn func(*convEntry)) *convEntry {
	id := conversationID(x.self, peer)
	e, ok := x.convs[id]
	var before Conversation
	if ok {
		before = e.conv
	} else {
		e = &convEntry{peer: peer, conv: Conversation{ID: id, PeerID: strconv.FormatInt(peer, 10)}}
		x.convs[id] = e
	}
	if fn != nil {
		fn(e)
	}
	if e.normalThread != 0 {
		x.threads[strconv.FormatInt(e.normalThread, 10)] = id
	}
	if e.e2eeThread != "" {
		x.threads[e.e2eeThread] = id
	}
	x.refresh(e)
	if !ok || !sameConversation(before, e.conv) {
		emit(Event{Type: EventConversationUpsert, Data: e.conv})
	}
	return e
}

func (x *ThreadIndex) applyLink(e *convEntry, armadillo int64) {
	e.e2eeThread = e2eeThreadID(armadillo)
	e.linked = true
}

func (x *ThreadIndex) touch(e *convEntry, ts int64, preview *string) {
	if ts >= e.conv.LastActivityTS {
		e.conv.LastActivityTS = ts
		if preview != nil {
			e.conv.LastMessagePreview = preview
		}
	}
}

func messagePreview(m Message) *string {
	if m.Text != nil {
		return m.Text
	}
	if len(m.Attachments) > 0 {
		p := "[" + m.Attachments[0].Kind + "]"
		return &p
	}
	return nil
}

type lsThread struct {
	key, activity, unread int64
	hasUnread             bool
	typ                   table.ThreadType
	name, picture         string
	snippet, folder       string
}

// Folders whose threads are not shown. mautrix never creates a portal for spam (events.go:676) and
// treats "pending" as a message request (chatinfo.go:253, events.go:685); the protocol has no message
// request flag, so those are hidden too until the thread moves to another folder. "other" is kept,
// as in mautrix.
const (
	folderSpam    = "spam"
	folderPending = "pending"
)

type resolveMode int

const (
	resolveOK       resolveMode = iota // create or update the conversation
	resolveExisting                    // route into an existing conversation only
	resolvePending                     // E2EE thread waiting for its JID mapping
	resolveIgnore                      // group or other non-1-1 thread
	resolveSkip                        // nothing known about the thread
)

// resolvePeer applies decision A. (1) The JID user from a hybrid mapping: handlemeta.go:347 maps
// ThreadKey to ThreadJID before anything else, and the LS key of an E2EE thread is not a user ID
// (seen live: a type-15 thread keyed 1147278207882860 mapped to the peer JID). (2) The non-self
// participant. (3) The thread key, for normal 1-1 threads only.
func (x *ThreadIndex) resolvePeer(t lsThread, participants map[int64][]int64) (peer int64, e2ee bool, mode resolveMode) {
	jid, mapped := x.hybrid[t.key]
	switch {
	case t.typ != table.UNKNOWN_THREAD_TYPE && !t.typ.IsOneToOne():
		return 0, false, resolveIgnore
	case mapped:
		// A mapping alone has no thread type; it creates a conversation only once 1-1 is confirmed by
		// LSVerifyHybridThreadExists or by a typed thread row.
		if t.typ != table.UNKNOWN_THREAD_TYPE || x.confirmed[t.key] {
			return jid, true, resolveOK
		}
		return jid, true, resolveExisting
	case t.typ == table.UNKNOWN_THREAD_TYPE:
		if id, ok := x.threads[strconv.FormatInt(t.key, 10)]; ok {
			return x.convs[id].peer, false, resolveExisting
		}
		return 0, false, resolveSkip
	}
	for _, p := range participants[t.key] {
		if p != x.self {
			return p, t.typ.IsWhatsApp(), resolveOK
		}
	}
	if t.typ.IsWhatsApp() {
		// The key of an E2EE thread is not the peer; wait for the mapping instead of guessing.
		return 0, true, resolvePending
	}
	return t.key, false, resolveOK
}

// ApplyTable folds a Lightspeed table into the index and emits conversation and message events.
// Order follows handlemeta.go:270-345: hybrid mappings first, then contacts, threads, links, messages.
func (x *ThreadIndex) ApplyTable(tbl *table.LSTable, emit func(Event)) {
	x.mu.Lock()
	defer x.mu.Unlock()

	for _, m := range tbl.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID {
		x.hybrid[m.ThreadKey] = m.ThreadJID
	}
	var verified []*table.LSVerifyHybridThreadExists
	for _, m := range tbl.LSVerifyHybridThreadExists {
		if m.IsGroupThread || (m.ThreadType != table.UNKNOWN_THREAD_TYPE && !m.ThreadType.IsOneToOne()) {
			x.ignored[m.ThreadKey] = true
			delete(x.hybrid, m.ThreadKey)
			continue
		}
		x.hybrid[m.ThreadKey] = m.ThreadJID
		if m.ThreadType.IsOneToOne() {
			x.confirmed[m.ThreadKey] = true
			verified = append(verified, m)
		}
	}

	changedContacts := map[int64]bool{}
	setContact := func(id int64, name, username, avatar string) {
		if name == "" {
			name = username
		}
		c := contactInfo{name: name, avatar: avatar}
		if old, ok := x.contacts[id]; !ok || old != c {
			x.contacts[id] = c
			changedContacts[id] = true
		}
	}
	for _, c := range tbl.LSDeleteThenInsertContact {
		setContact(c.GetFBID(), c.GetName(), c.GetUsername(), c.GetAvatarURL())
	}
	for _, c := range tbl.LSVerifyContactRowExists {
		setContact(c.GetFBID(), c.GetName(), c.GetUsername(), c.GetAvatarURL())
	}

	participants := map[int64][]int64{}
	for _, p := range tbl.LSAddParticipantIdToGroupThread {
		participants[p.ThreadKey] = append(participants[p.ThreadKey], p.ContactId)
	}

	// E2EE threads held back for a mapping go first, so rows in this table override them.
	var threads []lsThread
	for key, t := range x.pendingE2EE {
		if _, ok := x.hybrid[key]; ok {
			threads = append(threads, t)
			delete(x.pendingE2EE, key)
		}
	}
	for _, t := range tbl.LSDeleteThenInsertThread {
		threads = append(threads, lsThread{key: t.ThreadKey, typ: t.ThreadType, activity: t.LastActivityTimestampMs,
			unread: t.UnreadMessageCount, hasUnread: true, name: t.ThreadName, picture: t.ThreadPictureUrl,
			snippet: t.Snippet, folder: t.FolderName})
	}
	for _, t := range tbl.LSUpdateOrInsertThread {
		threads = append(threads, lsThread{key: t.ThreadKey, typ: t.ThreadType, activity: t.LastActivityTimestampMs,
			name: t.ThreadName, picture: t.ThreadPictureUrl, snippet: t.Snippet, folder: t.FolderName})
	}
	for _, t := range tbl.LSVerifyThreadExists {
		threads = append(threads, lsThread{key: t.ThreadKey, typ: t.ThreadType, folder: t.FolderName})
	}
	for _, t := range threads {
		if t.folder == folderSpam || t.folder == folderPending {
			x.ignored[t.key] = true
			delete(x.pendingE2EE, t.key)
			continue
		}
		peer, e2ee, mode := x.resolvePeer(t, participants)
		switch mode {
		case resolveIgnore:
			x.ignored[t.key] = true
			continue
		case resolvePending:
			x.pendingE2EE[t.key] = t
			continue
		case resolveSkip:
			x.log.Warn().Msg("Skipping thread with unknown type")
			continue
		case resolveExisting:
			if _, ok := x.convs[conversationID(x.self, peer)]; !ok {
				continue
			}
		case resolveOK:
			delete(x.ignored, t.key)
		}
		x.update(peer, emit, func(e *convEntry) {
			if e2ee {
				e.e2eeThread = e2eeThreadID(peer)
				e.lsE2EEKey = t.key
				x.threads[strconv.FormatInt(t.key, 10)] = e.conv.ID
			} else if mode == resolveOK {
				e.normalThread = t.key
			}
			if armadillo, ok := x.pending[t.key]; ok {
				x.applyLink(e, armadillo)
				delete(x.pending, t.key)
			}
			// 1-1 thread names are the peer's name; contacts still take precedence in displayName.
			if t.name != "" {
				e.threadName = t.name
			}
			if t.picture != "" {
				e.threadPicture = t.picture
			}
			if t.activity > 0 {
				x.touch(e, t.activity, optString(t.snippet))
			}
			if t.hasUnread {
				e.conv.UnreadCount = int(t.unread)
			}
		})
	}
	for _, m := range verified {
		if x.ignored[m.ThreadKey] {
			continue
		}
		jid, key, ts := m.ThreadJID, m.ThreadKey, m.LastActivityTimestampMS
		x.update(jid, emit, func(e *convEntry) {
			e.e2eeThread = e2eeThreadID(jid)
			e.lsE2EEKey = key
			x.threads[strconv.FormatInt(key, 10)] = e.conv.ID
			x.touch(e, ts, nil)
		})
	}

	// history_cutover.go stores the same link; here it only reroutes sends to the E2EE thread.
	for _, l := range tbl.LSCreateOpenToE2EEThreadLink {
		if x.ignored[l.OpenThreadID] || x.ignored[l.ArmadilloThreadID] {
			continue
		}
		if id, ok := x.threads[strconv.FormatInt(l.OpenThreadID, 10)]; ok {
			x.applyLink(x.convs[id], l.ArmadilloThreadID)
			x.threads[x.convs[id].e2eeThread] = id
		} else if id, ok := x.threads[e2eeThreadID(l.ArmadilloThreadID)]; ok {
			x.convs[id].normalThread = l.OpenThreadID
			x.convs[id].linked = true
			x.threads[strconv.FormatInt(l.OpenThreadID, 10)] = id
		} else {
			x.pending[l.OpenThreadID] = l.ArmadilloThreadID
		}
	}

	for id := range changedContacts {
		if e, ok := x.convs[conversationID(x.self, id)]; ok {
			x.update(e.peer, emit, nil)
		}
	}

	upsert, insert := tbl.WrapMessages()
	var msgs []*table.WrappedMessage
	keys := make([]int64, 0, len(upsert))
	for k := range upsert {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		if x.collect(k, upsert[k]) || x.historyIsMuted(k) {
			continue
		}
		msgs = append(msgs, upsert[k].Messages...)
	}
	msgs = append(msgs, insert...)
	for _, msg := range msgs {
		x.applyLSMessage(msg, emit)
	}
	// backfill.go handleUpdateExistingMessageRange: the server answers a batch with nothing before the
	// anchor by updating the range instead of upserting messages.
	for _, rng := range tbl.LSUpdateExistingMessageRange {
		if c := x.collectors[rng.ThreadKey]; c != nil && c.anchorTS == rng.TimestampMS {
			x.collect(rng.ThreadKey, &table.UpsertMessages{Range: &table.LSInsertNewMessageRange{ThreadKey: rng.ThreadKey}})
		}
	}
}

func (x *ThreadIndex) convForLSThread(key, sender int64, emit func(Event)) *convEntry {
	if x.ignored[key] {
		return nil
	}
	if jid, ok := x.hybrid[key]; ok {
		if id, ok := x.threads[e2eeThreadID(jid)]; ok {
			return x.convs[id]
		}
		return nil
	}
	if id, ok := x.threads[strconv.FormatInt(key, 10)]; ok {
		return x.convs[id]
	}
	if _, ok := x.pendingE2EE[key]; ok {
		return nil
	}
	// A 1-1 thread key is the other person's ID, while group keys never equal a user ID. This covers a
	// first message from someone whose thread row has not arrived (collectPortalEvents would use an
	// uncertain receiver here).
	if sender != 0 && sender != x.self && sender == key {
		return x.update(key, emit, func(e *convEntry) { e.normalThread = key })
	}
	return nil
}

func (x *ThreadIndex) applyLSMessage(msg *table.WrappedMessage, emit func(Event)) {
	if msg.LSInsertMessage == nil || msg.MessageId == "" {
		return
	}
	if _, seen := x.seen[msg.MessageId]; seen {
		return
	}
	var clientID *string
	if c, ok := x.otidClient[msg.OfflineThreadingId]; ok && msg.OfflineThreadingId != "" {
		clientID = &c
	}
	// Convert before resolving the thread, so a skipped admin or XMA-only message never creates one.
	m, ok := fromLS(msg, "", x.self, clientID)
	if !ok {
		x.log.Debug().Bool("admin", msg.IsAdminMessage).Int("xma", len(msg.XMAAttachments)).Msg("Skipping unsupported message")
		return
	}
	e := x.convForLSThread(msg.ThreadKey, msg.SenderId, emit)
	if e == nil {
		if !x.ignored[msg.ThreadKey] {
			x.log.Warn().Msg("Skipping message in unknown thread")
		}
		return
	}
	x.markSeen(msg.MessageId)
	if msg.ThreadKey == e.normalThread {
		e.oldestLS.lower(m.TS, m.ID)
	}
	m.ConversationID = e.conv.ID
	x.update(e.peer, emit, func(e *convEntry) { x.touch(e, m.TS, messagePreview(m)) })
	emit(Event{Type: EventMessageUpsert, Data: m})
}

// ApplyWAMessage handles an E2EE message. A chat known only from E2EE is created from the JID user,
// named by the peer ID until a contact row arrives (decision D).
func (x *ThreadIndex) ApplyWAMessage(evt *events.FBMessage, emit func(Event)) {
	chat := evt.Info.Chat
	if evt.Info.IsGroup || chat.Server != waTypes.MessengerServer {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	peer, err := strconv.ParseInt(chat.User, 10, 64)
	if err != nil || peer <= 0 {
		x.log.Warn().Msg("Skipping E2EE message with unparseable chat")
		return
	}
	if _, seen := x.seen[evt.Info.ID]; seen {
		return
	}
	var clientID *string
	if c, ok := x.waClient[evt.Info.ID]; ok {
		clientID = &c
	}
	id := conversationID(x.self, peer)
	m, ok := fromWA(evt, id, x.self, clientID)
	if !ok {
		return
	}
	x.markSeen(evt.Info.ID)
	x.update(peer, emit, func(e *convEntry) {
		e.e2eeThread = chat.ToNonAD().String()
		e.oldestWA.lower(m.TS, m.ID)
		x.touch(e, m.TS, messagePreview(m))
	})
	if !m.IsOutgoing {
		list := append(x.waUnread[id], waIncoming{id: m.ID, ts: m.TS})
		if len(list) > waUnreadCapacity {
			list = list[len(list)-waUnreadCapacity:]
		}
		x.waUnread[id] = list
	}
	emit(Event{Type: EventMessageUpsert, Data: m})
}

// readTargets are the threads mark_read writes to for one conversation.
type readTargets struct {
	normalThread int64       // Lightspeed thread, sync group 1
	e2eeLSKey    int64       // Lightspeed key of the E2EE thread, sync group 95
	e2eeChat     waTypes.JID // E2EE chat to send read receipts in
	waIDs        []string    // incoming E2EE messages at or before until_ts
	waTaken      []waIncoming
}

// takeReadTargets returns where to mark convID read up to untilTS. The returned E2EE message IDs are
// removed from the index, so each receipt is sent once.
func (x *ThreadIndex) takeReadTargets(convID string, untilTS int64) (readTargets, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.convs[convID]
	if e == nil {
		return readTargets{}, false
	}
	t := readTargets{normalThread: e.normalThread, e2eeLSKey: e.lsE2EEKey}
	if e.e2eeThread != "" {
		if jid, err := waTypes.ParseJID(e.e2eeThread); err == nil {
			t.e2eeChat = jid
		}
	}
	var keep []waIncoming
	for _, m := range x.waUnread[convID] {
		if m.ts <= untilTS {
			t.waIDs = append(t.waIDs, m.id)
			t.waTaken = append(t.waTaken, m)
		} else {
			keep = append(keep, m)
		}
	}
	if len(keep) == 0 {
		delete(x.waUnread, convID)
	} else {
		x.waUnread[convID] = keep
	}
	return t, true
}

// restoreUnread puts back E2EE message IDs whose read receipt failed, ahead of the newer ones, keeping
// the newest waUnreadCapacity.
func (x *ThreadIndex) restoreUnread(convID string, taken []waIncoming) {
	if len(taken) == 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.convs[convID] == nil {
		return
	}
	list := append(append([]waIncoming(nil), taken...), x.waUnread[convID]...)
	if len(list) > waUnreadCapacity {
		list = list[len(list)-waUnreadCapacity:]
	}
	x.waUnread[convID] = list
}

func (x *ThreadIndex) setLogger(log zerolog.Logger) {
	x.mu.Lock()
	x.log = log.With().Str("component", "conversations").Logger()
	x.mu.Unlock()
}

const (
	e2eeSyncGroup       = 95
	threadPagesOnStart  = 3
	threadPageDelay     = time.Second
	threadRangeTimeout  = 30 * time.Second
	initialRangeTimeout = 5 * time.Second
	// snapshotE2EEThreads waits for the client state at most this many times, this far apart.
	snapshotStateAttempts = 5
	snapshotStateDelay    = 3 * time.Second
)

// sink returns the login emitter while client is the live, authenticated session, nil otherwise.
func (b *MetaBackend) sink(client metaClient) func(Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != client || b.auth || b.emit == nil {
		return nil
	}
	return b.emit
}

func (b *MetaBackend) handleTable(client metaClient, tbl *table.LSTable) {
	b.mu.Lock()
	var emit func(Event)
	var index *ThreadIndex
	if b.client == client && !b.auth {
		emit, index = b.emit, b.index
	}
	b.mu.Unlock()
	// Send confirmations and refreshed media URLs may arrive over the socket instead of in the response.
	b.waits.observe(tbl)
	b.refreshes.observe(tbl)
	if emit != nil && index != nil {
		index.ApplyTable(tbl, emit)
	}
}

// onConnected runs once per session, like the ConnectedEvent branch of handlemeta.go:181: handle the
// inbox table, start E2EE and page through the E2EE thread list.
func (b *MetaBackend) onConnected(client metaClient) {
	b.mu.Lock()
	if b.client != client || b.auth || b.connected {
		b.mu.Unlock()
		return
	}
	b.connected = true
	e2ee, ctx := b.e2ee, b.ctx
	b.mu.Unlock()
	if rc, ok := client.(*realMetaClient); ok {
		if tbl := rc.initial.Swap(nil); tbl != nil {
			b.handleTable(client, tbl)
		}
		go b.fetchE2EEThreads(ctx, client, rc)
	}
	if e2ee != nil {
		e2ee.start()
	}
	b.announceHistory(client)
}

// fetchE2EEThreads loads the E2EE (group 95) thread list after connecting, following
// threadbackfill.go:36 but capped at a few pages so the startup stays light (decision C).
func (b *MetaBackend) fetchE2EEThreads(ctx context.Context, client metaClient, rc *realMetaClient) {
	if ctx == nil {
		return
	}
	b.mu.Lock()
	index := b.index
	b.mu.Unlock()
	if index == nil {
		return
	}
	index.mu.Lock()
	log := index.log
	index.mu.Unlock()
	wait := func(timeout time.Duration, f func(context.Context) error) error {
		wctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return f(wctx)
	}
	// A timeout is not fatal: when the page load falls back to the GraphQL sync, the initial
	// group-95 range never arrives (seen with an account that only has E2EE chats).
	_ = wait(initialRangeTimeout, func(c context.Context) error { return rc.WaitForInitialThreadRange(c, e2eeSyncGroup) })
	for page := 0; page < threadPagesOnStart; page++ {
		select {
		case <-time.After(threadPageDelay):
		case <-ctx.Done():
			return
		}
		if b.sink(client) == nil {
			return
		}
		// fetch_conversations pages the same key store; never request the same page twice at once.
		b.threadPaging.Lock()
		stop := b.fetchE2EEThreadPage(ctx, client, rc, page, wait, log)
		b.threadPaging.Unlock()
		if stop {
			return
		}
	}
}

// fetchE2EEThreadPage loads one page of fetchE2EEThreads; true ends the paging.
func (b *MetaBackend) fetchE2EEThreadPage(ctx context.Context, client metaClient, rc *realMetaClient, page int,
	wait func(time.Duration, func(context.Context) error) error, log zerolog.Logger) bool {
	keyStore := rc.GetSyncGroupKeyStore(e2eeSyncGroup)
	if page == 0 && (keyStore == nil || (!keyStore.HasMoreBefore && keyStore.MinThreadKey == 0)) {
		// The key store was never filled, so FetchMoreThreads would return nothing. messagix keeps
		// only the cutover links of the db-95 snapshot it fetches at page load (syncManager.go:262);
		// fetch that snapshot again and keep the whole table. It is not paged.
		tbl, err := snapshotE2EEThreads(ctx, rc)
		if err != nil {
			log.Error().Err(err).Msg("Failed to fetch E2EE thread snapshot")
		} else if tbl != nil {
			b.handleTable(client, tbl)
		}
		return true
	}
	if keyStore == nil || !keyStore.HasMoreBefore {
		return true
	}
	prevMin := keyStore.MinThreadKey
	gen := rc.ThreadRangeGeneration(e2eeSyncGroup)
	_, tbl, err := rc.FetchMoreThreads(ctx, e2eeSyncGroup)
	if err != nil {
		log.Error().Err(err).Msg("Failed to fetch more E2EE threads")
		return true
	} else if tbl == nil {
		return true
	}
	b.handleTable(client, tbl)
	if err = wait(threadRangeTimeout, func(c context.Context) error { return rc.WaitForThreadRange(c, e2eeSyncGroup, gen) }); err != nil {
		return true
	}
	return keyStore.HasMoreBefore && keyStore.MinThreadKey == prevMin
}

// snapshotE2EEThreads repeats the db-95 request of SyncDataGraphQL. The sync cursors are not touched:
// the socket keeps syncing from where messagix left off.
func snapshotE2EEThreads(ctx context.Context, rc *realMetaClient) (*table.LSTable, error) {
	// DumpState is nil until messagix marks the socket synced, which can trail ConnectedEvent.
	var raw json.RawMessage
	var err error
	for attempt := 0; ; attempt++ {
		if raw, err = rc.DumpState(); err != nil {
			return nil, fmt.Errorf("dump client state: %w", err)
		} else if raw != nil {
			break
		} else if attempt+1 >= snapshotStateAttempts {
			return nil, errors.New("client state unavailable")
		}
		select {
		case <-time.After(snapshotStateDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var state struct {
		Configs struct {
			VersionID          int64
			BrowserConfigTable *struct {
				LSPlatformMessengerSyncParams types.LSPlatformMessengerSyncParams
			}
		}
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		return nil, err
	} else if state.Configs.BrowserConfigTable == nil {
		return nil, errors.New("client config unavailable")
	}
	params := state.Configs.BrowserConfigTable.LSPlatformMessengerSyncParams.Contact
	return rc.GetHTTP().MakeLSRequest(ctx, &graphql.LSPlatformGraphQLLightspeedVariables{
		Database:   e2eeSyncGroup,
		Version:    state.Configs.VersionID,
		SyncParams: &params,
	}, 1)
}

// ListConversations replays every known conversation; the cursor is ignored (decision I).
func (b *MetaBackend) ListConversations(_ context.Context, _ ListConversationsArgs, emit func(Event)) error {
	b.mu.Lock()
	index := b.index
	b.mu.Unlock()
	if index == nil {
		return &Error{Code: "disconnected", Message: "no active session"}
	}
	if emit != nil {
		for _, c := range index.Conversations() {
			emit(Event{Type: EventConversationUpsert, Data: c})
		}
	}
	return nil
}
