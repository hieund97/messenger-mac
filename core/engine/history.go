package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	waTypes "go.mau.fi/whatsmeow/types"
)

const (
	defaultFetchLimit = 20
	maxFetchLimit     = 100
	// maxHistoryRequests bounds the requests one fetch_messages makes per thread.
	maxHistoryRequests = 5
	// historyBatchTimeout is backfill.go BackfillTimeout: how long a batch may take to arrive.
	historyBatchTimeout = 30 * time.Second
	historyRetryDelay   = 5 * time.Second
	// defaultPagingDelay spaces consecutive history batches of one thread and thread list pages.
	defaultPagingDelay = time.Second
)

var errHistoryTimeout = &Error{Code: "timeout", Message: "the server did not send the history in time"}

// fetchCursor is the opaque cursor of fetch_messages: one position per thread of the conversation.
// Swift stores it and sends it back unchanged.
type fetchCursor struct {
	LS *lsCursor `json:"ls,omitempty"`
	WA *waCursor `json:"wa,omitempty"`
}

// lsCursor is the oldest message reached in the normal thread, the anchor of the next
// FetchMessagesTask (backfill.go keeps the same timestamp and message ID pair).
type lsCursor struct {
	Thread int64  `json:"t"`
	TS     int64  `json:"ts,omitempty"`
	ID     string `json:"id,omitempty"`
	End    bool   `json:"end,omitempty"`
}

// waCursor is the position in the secure history of the E2EE thread. The range query takes only a
// timestamp, so the IDs already returned at TS are kept to drop them when the next page repeats them.
// Strict means the next request excludes TS (see fetchWA).
type waCursor struct {
	Chat   string   `json:"c"`
	TS     int64    `json:"ts,omitempty"`
	IDs    []string `json:"ids,omitempty"`
	Strict bool     `json:"strict,omitempty"`
	End    bool     `json:"end,omitempty"`
}

func encodeCursor(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses a cursor that came back from Swift; it is untrusted.
func decodeCursor(s *string, v any) error {
	if s == nil || *s == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(*s)
	if err == nil {
		err = json.Unmarshal(raw, v)
	}
	if err != nil {
		return &Error{Code: "bad_request", Message: "invalid cursor"}
	}
	return nil
}

// withSession returns a context that ends with the command and with the session.
func withSession(cmd, session context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(session)
	stop := context.AfterFunc(cmd, cancel)
	return ctx, func() { stop(); cancel() }
}

// loginEmit is the login emitter while the session is authenticated, a no-op otherwise.
func (b *MetaBackend) loginEmit() func(Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.emit == nil || b.auth {
		return func(Event) {}
	}
	return b.emit
}

// waitPagingSlot spaces requests sharing key by at least pagingDelay, across commands too. The slot is
// reserved before waiting, so concurrent callers queue up instead of firing together.
func (b *MetaBackend) waitPagingSlot(ctx context.Context, key string) error {
	delay := b.pagingDelay
	if delay <= 0 {
		delay = defaultPagingDelay
	}
	b.pagingMu.Lock()
	if b.lastPage == nil {
		b.lastPage = map[string]time.Time{}
	}
	now := time.Now()
	next := now
	if last, ok := b.lastPage[key]; ok && last.Add(delay).After(now) {
		next = last.Add(delay)
	}
	b.lastPage[key] = next
	b.pagingMu.Unlock()
	if wait := time.Until(next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// claimFetch allows one fetch_messages per conversation at a time.
func (b *MetaBackend) claimFetch(convID string) bool {
	b.pagingMu.Lock()
	defer b.pagingMu.Unlock()
	if b.fetching == nil {
		b.fetching = map[string]bool{}
	}
	if b.fetching[convID] {
		return false
	}
	b.fetching[convID] = true
	return true
}

func (b *MetaBackend) releaseFetch(convID string) {
	b.pagingMu.Lock()
	defer b.pagingMu.Unlock()
	delete(b.fetching, convID)
}

// historyTarget is what fetch_messages needs to know about a conversation.
type historyTarget struct {
	normalThread int64
	e2eeChat     string // "<peer>@msgr", empty without an E2EE thread
	oldestLS     msgPos
	oldestWA     msgPos
}

func (x *ThreadIndex) historyTarget(convID string) (historyTarget, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.convs[convID]
	if e == nil {
		return historyTarget{}, false
	}
	return historyTarget{normalThread: e.normalThread, e2eeChat: e.e2eeThread, oldestLS: e.oldestLS, oldestWA: e.oldestWA}, true
}

// lsHistory converts a history batch of the normal thread, leaving out messages already emitted.
// Nothing is marked: fetchMessages decides which messages it emits (markEmitted).
func (x *ThreadIndex) lsHistory(convID string, batch []*table.WrappedMessage) []Message {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.convs[convID] == nil {
		return nil
	}
	var out []Message
	for _, msg := range batch {
		if msg.LSInsertMessage == nil || msg.MessageId == "" {
			continue
		}
		if _, seen := x.seen[msg.MessageId]; seen {
			continue
		}
		var clientID *string
		if c, ok := x.otidClient[msg.OfflineThreadingId]; ok && msg.OfflineThreadingId != "" {
			clientID = &c
		}
		if m, ok := fromLS(msg, convID, x.self, clientID); ok {
			out = append(out, m)
		}
	}
	return out
}

// waHistory keeps the secure history messages that were not emitted yet, without marking them.
func (x *ThreadIndex) waHistory(convID string, msgs []Message, skip map[string]bool) []Message {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.convs[convID] == nil {
		return nil
	}
	var out []Message
	for _, m := range msgs {
		if _, seen := x.seen[m.ID]; seen || skip[m.ID] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// markEmitted records the history messages fetch_messages emits: they are never emitted again, and
// the oldest of each thread is where a fetch without a cursor starts.
func (x *ThreadIndex) markEmitted(convID string, msgs []Message) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.convs[convID]
	normal := ""
	if e != nil && e.normalThread != 0 {
		normal = strconv.FormatInt(e.normalThread, 10)
	}
	for _, m := range msgs {
		x.markSeen(m.ID)
		switch {
		case e == nil:
		case m.E2EE:
			e.oldestWA.lower(m.TS, m.ID)
		case m.ThreadID == normal:
			e.oldestLS.lower(m.TS, m.ID)
		}
	}
}

// FetchMessages pages backwards through a conversation: message_upsert events newest first, then
// response{ok, data:{has_more, cursor[, needs_restore]}}. A conversation with both a normal and an
// E2EE thread pages both and merges them by timestamp. Messages already emitted are never repeated.
func (b *MetaBackend) FetchMessages(ctx context.Context, args FetchMessagesArgs, emit func(Event)) error {
	data, err := b.fetchMessages(ctx, args, emit)
	if err != nil {
		return err
	}
	return setResponseData(ctx, data)
}

type fetchMessagesResult struct {
	HasMore      bool   `json:"has_more"`
	Cursor       string `json:"cursor"`
	NeedsRestore bool   `json:"needs_restore,omitempty"`
	// Count is the number of message_upsert events emitted before the response. Swift receives those
	// events on a separate stream, so it cannot tell from its cache whether they have arrived yet.
	Count int `json:"count"`
}

// recency orders thread positions for paging: a position without a timestamp has not fetched
// anything yet, so it is the most recent.
func recency(ts int64) int64 {
	if ts <= 0 {
		return math.MaxInt64
	}
	return ts
}

func (b *MetaBackend) fetchMessages(cmdCtx context.Context, args FetchMessagesArgs, emit func(Event)) (json.RawMessage, error) {
	if args.ConversationID == "" {
		return nil, &Error{Code: "bad_request", Message: "conversation_id is required"}
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultFetchLimit
	}
	limit = min(limit, maxFetchLimit)
	var cur fetchCursor
	if err := decodeCursor(args.Cursor, &cur); err != nil {
		return nil, err
	}
	s, err := b.liveSession()
	if err != nil {
		return nil, err
	}
	t, ok := s.index.historyTarget(args.ConversationID)
	if !ok {
		return nil, &Error{Code: "unknown_conversation", Message: "unknown conversation"}
	}
	if !b.claimFetch(args.ConversationID) {
		return nil, &Error{Code: "busy", Message: "this conversation is already being fetched"}
	}
	defer b.releaseFetch(args.ConversationID)
	ctx, cancel := withSession(cmdCtx, s.ctx)
	defer cancel()

	var (
		result   fetchMessagesResult
		next     fetchCursor
		firstErr error
		// Fetched but not emitted yet, per thread, without duplicates.
		lsMsgs, waMsgs []Message
		buffered       = map[string]bool{}
		// A thread that failed keeps its position and is not asked again in this call.
		lsFailed, waFailed bool
		waOps              historyOps
		chat               waTypes.JID
	)
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	add := func(list *[]Message, got []Message) {
		for _, m := range got {
			if !buffered[m.ID] {
				buffered[m.ID] = true
				*list = append(*list, m)
			}
		}
	}
	if t.normalThread != 0 {
		pos := lsCursor{Thread: t.normalThread, TS: t.oldestLS.ts, ID: t.oldestLS.id}
		if cur.LS != nil && cur.LS.Thread == t.normalThread {
			pos = *cur.LS
		}
		next.LS = &pos
	}
	if t.e2eeChat != "" {
		if device := b.history.device(); device == nil {
			// Without the secure history the E2EE thread has no history to page through.
			result.NeedsRestore = true
		} else {
			pos := waCursor{Chat: t.e2eeChat}
			if cur.WA != nil && cur.WA.Chat == t.e2eeChat {
				pos = *cur.WA
			} else if t.oldestWA.id != "" {
				pos.TS, pos.IDs = t.oldestWA.ts, []string{t.oldestWA.id}
			}
			next.WA = &pos
			if chat, err = waTypes.ParseJID(pos.Chat); err != nil {
				fail(&Error{Code: "internal", Message: "invalid E2EE thread"})
				waFailed = true
			} else if waOps, err = b.historyOps(); err != nil {
				fail(err)
				waFailed = true
			}
		}
	}
	lsActive := func() bool { return next.LS != nil && !lsFailed && !next.LS.End }
	waActive := func() bool { return next.WA != nil && !waFailed && !next.WA.End }
	// Messages at or after the frontier are complete across the threads still being paged; an older
	// one of a thread may still have messages of the other thread after it that were not fetched yet.
	frontier := func() int64 {
		f := int64(math.MinInt64)
		if lsActive() {
			f = max(f, recency(next.LS.TS))
		}
		if waActive() {
			f = max(f, recency(next.WA.TS))
		}
		return f
	}
	emittable := func() int {
		f, n := frontier(), 0
		for _, list := range [][]Message{lsMsgs, waMsgs} {
			for _, m := range list {
				if m.TS >= f {
					n++
				}
			}
		}
		return n
	}
	// Both threads move back in time together: each request goes to the thread whose position is the
	// most recent (one that has not fetched anything yet first).
	for req := 0; req < maxHistoryRequests && emittable() < limit; req++ {
		useLS, useWA := lsActive(), waActive()
		if useLS && useWA {
			useLS = recency(next.LS.TS) >= recency(next.WA.TS)
			useWA = !useLS
		}
		if useLS {
			got, pos, err := b.fetchLS(ctx, s, args.ConversationID, *next.LS)
			*next.LS = pos
			add(&lsMsgs, got)
			if err != nil {
				fail(err)
				lsFailed = true
			}
		} else if useWA {
			got, pos, err := b.fetchWA(ctx, s, args.ConversationID, waOps, chat, *next.WA, limit)
			*next.WA = pos
			add(&waMsgs, got)
			if err != nil {
				fail(err)
				waFailed = true
			}
		} else {
			break
		}
	}
	// Messages older than the frontier are held back, and their thread is moved back to the newest of
	// them: a normal-thread batch includes its anchor message and an E2EE request includes the
	// reference timestamp, so they come again in a later page, after the other thread caught up.
	f := frontier()
	var msgs []Message
	hold := func(list []Message) *Message {
		var newest *Message
		for i, m := range list {
			if m.TS >= f {
				msgs = append(msgs, m)
			} else if newest == nil || m.TS > newest.TS || (m.TS == newest.TS && m.ID > newest.ID) {
				newest = &list[i]
			}
		}
		return newest
	}
	if m := hold(lsMsgs); m != nil {
		*next.LS = lsCursor{Thread: next.LS.Thread, TS: m.TS, ID: m.ID}
	}
	if m := hold(waMsgs); m != nil {
		*next.WA = waCursor{Chat: next.WA.Chat, TS: m.TS}
	}
	result.HasMore = (next.LS != nil && !next.LS.End) || (next.WA != nil && !next.WA.End)
	if s.ctx.Err() != nil {
		return nil, &Error{Code: "disconnected", Message: "session ended"}
	}
	if firstErr != nil && (len(msgs) == 0 || isNeedsRestore(firstErr)) {
		return nil, firstErr
	}
	s.index.markEmitted(args.ConversationID, msgs)
	sort.SliceStable(msgs, func(i, j int) bool {
		if msgs[i].TS != msgs[j].TS {
			return msgs[i].TS > msgs[j].TS
		}
		return msgs[i].ID > msgs[j].ID
	})
	for _, m := range msgs {
		emit(Event{Type: EventMessageUpsert, Data: m})
	}
	result.Count = len(msgs)
	result.Cursor = encodeCursor(next)
	return json.Marshal(result)
}

// fetchLS requests one batch of the normal thread with FetchMessagesTask (backfill.go
// fetchFacebookHistory). Batches of a thread are at least pagingDelay apart.
func (b *MetaBackend) fetchLS(ctx context.Context, s liveSession, convID string, pos lsCursor) ([]Message, lsCursor, error) {
	if s.ls == nil {
		return nil, pos, &Error{Code: "disconnected", Message: "no active session"}
	}
	if err := b.waitPagingSlot(ctx, "ls:"+strconv.FormatInt(pos.Thread, 10)); err != nil {
		return nil, pos, err
	}
	upsert, err := b.requestLSBatch(ctx, s, pos)
	if err != nil {
		return nil, pos, err
	}
	out := s.index.lsHistory(convID, upsert.Messages)
	rng := upsert.Range
	moved := rng.MinMessageId != "" && rng.MinMessageId != pos.ID && (pos.ID == "" || rng.MinTimestampMs <= pos.TS)
	if moved {
		pos.TS, pos.ID = rng.MinTimestampMs, rng.MinMessageId
	} else if rng.HasMoreBefore {
		// backfill.go fails with "history cursor did not move backwards"; stopping avoids asking
		// for the same batch forever.
		logger := s.index.logger()
		logger.Warn().Msg("History cursor did not move backwards")
	}
	pos.End = !rng.HasMoreBefore || !moved
	return out, pos, nil
}

// requestLSBatch asks for the batch before pos and waits for it, from the response or the socket
// (backfill.go requestMoreHistory and the collector). Without an anchor (no message of the thread in
// memory) it sends CreateThreadTask like requestMoreHistory does.
func (b *MetaBackend) requestLSBatch(ctx context.Context, s liveSession, pos lsCursor) (*table.UpsertMessages, error) {
	c, ok := s.index.addCollector(pos.Thread, pos.TS)
	if !ok {
		return nil, &Error{Code: "busy", Message: "this conversation is already being fetched"}
	}
	defer s.index.removeCollector(pos.Thread, c)
	var task socket.Task = &socket.FetchMessagesTask{
		ThreadKey:            pos.Thread,
		Direction:            0,
		ReferenceTimestampMs: pos.TS,
		ReferenceMessageId:   pos.ID,
		SyncGroup:            1,
		Cursor:               s.ls.Cursor(1),
	}
	if pos.ID == "" {
		task = &socket.CreateThreadTask{ThreadFBID: pos.Thread, SyncGroup: 1}
	}
	reqCtx, cancel := context.WithTimeout(ctx, historyBatchTimeout)
	defer cancel()
	type taskResult struct {
		table *table.LSTable
		err   error
	}
	results := make(chan taskResult, 2)
	send := func() { go func() { tbl, err := s.ls.ExecuteTasks(reqCtx, task); results <- taskResult{tbl, err} }() }
	send()
	timer := time.NewTimer(historyRetryDelay)
	defer timer.Stop()
	for {
		select {
		case result := <-results:
			if result.err != nil {
				if s.ctx.Err() != nil {
					return nil, &Error{Code: "disconnected", Message: "session ended"}
				}
				if reqCtx.Err() != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return nil, errHistoryTimeout
				}
				return nil, &Error{Code: reasonCode(result.err), Message: "failed to fetch history"}
			}
			if result.table != nil {
				b.applyFetchedTable(s, result.table, b.loginEmit())
			}
		case upsert := <-c.ch:
			if upsert.Range == nil {
				upsert.Range = &table.LSInsertNewMessageRange{ThreadKey: pos.Thread}
			}
			return upsert, nil
		case <-timer.C:
			send()
		case <-reqCtx.Done():
			if s.ctx.Err() != nil {
				return nil, &Error{Code: "disconnected", Message: "session ended"}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errHistoryTimeout
		}
	}
}

// applyFetchedTable handles a table from a direct response like one from the socket.
func (b *MetaBackend) applyFetchedTable(s liveSession, tbl *table.LSTable, emit func(Event)) {
	b.waits.observe(tbl)
	b.refreshes.observe(tbl)
	s.index.ApplyTable(tbl, emit)
}

// fetchWA requests one page of the secure history of the E2EE thread (backfill_e2ee.go / history.go
// fetchEncryptedHistory). The range query takes only a reference timestamp, so it is asked for
// "before pos.TS+1" to include messages sharing the timestamp of the oldest one seen, and the ones
// already emitted are dropped by ID. If a whole page is already known (more messages at one timestamp
// than a page holds), the next request excludes pos.TS so paging never loops; messages at that
// timestamp beyond the page can then be missed. This is accepted as best effort.
func (b *MetaBackend) fetchWA(ctx context.Context, s liveSession, convID string, ops historyOps, chat waTypes.JID, pos waCursor, count int) ([]Message, waCursor, error) {
	device := b.history.device()
	if device == nil {
		return nil, pos, &Error{Code: "history_not_restored", Message: "encrypted history is not restored"}
	}
	if err := b.waitPagingSlot(ctx, "wa:"+pos.Chat); err != nil {
		return nil, pos, err
	}
	reference := time.Now().Add(time.Minute).UnixMilli()
	if pos.TS > 0 {
		reference = pos.TS + 1
		if pos.Strict {
			reference = pos.TS
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, historyBatchTimeout)
	page, err := ops.FetchRange(reqCtx, device, chat.User, reference, count)
	cancel()
	if err != nil {
		if s.ctx.Err() != nil {
			return nil, pos, &Error{Code: "disconnected", Message: "session ended"}
		}
		if invalidHistoryDevice(err) {
			b.invalidateHistory(s, device)
			return nil, pos, &Error{Code: "needs_restore", Message: "encrypted history needs to be restored"}
		}
		return nil, pos, &Error{Code: "history_failed", Message: "failed to fetch encrypted history"}
	}
	updated, err := b.applyHistoryEpochs(device, page.Epochs)
	if err != nil {
		if invalidHistoryDevice(err) {
			b.invalidateHistory(s, device)
			return nil, pos, &Error{Code: "needs_restore", Message: "encrypted history needs to be restored"}
		}
		return nil, pos, &Error{Code: "history_failed", Message: "failed to update encrypted history keys"}
	}
	device = updated
	decrypted := page.Decrypt(ctx, device, chat.User, s.index.self, false)
	skip := map[string]bool{}
	for _, id := range pos.IDs {
		skip[id] = true
	}
	var converted []Message
	oldest, oldestIDs := int64(0), []string(nil)
	for _, dm := range decrypted {
		if dm == nil || dm.Message == nil {
			continue
		}
		ts := dm.Message.TimestampMS
		if oldest == 0 || ts < oldest {
			oldest, oldestIDs = ts, nil
		}
		if ts == oldest {
			oldestIDs = append(oldestIDs, dm.ID)
		}
		if m, ok := archiveMessage(dm, chat, convID, s.index.self); ok {
			converted = append(converted, m)
		}
	}
	got := s.index.waHistory(convID, converted, skip)
	more := page.Range.HasMoreBefore != nil && *page.Range.HasMoreBefore
	switch {
	case len(decrypted) > 0 && (len(got) > 0 || pos.TS == 0 || oldest < pos.TS):
		if oldest == pos.TS {
			oldestIDs = append(oldestIDs, pos.IDs...)
		}
		pos = waCursor{Chat: pos.Chat, TS: oldest, IDs: oldestIDs}
	case more && pos.TS > 0 && !pos.Strict:
		pos.Strict = true
	case more && page.Range.Before > 0 && (pos.TS == 0 || page.Range.Before < pos.TS):
		// Nothing decryptable in this page: continue from where the server says the next one is.
		pos = waCursor{Chat: pos.Chat, TS: page.Range.Before}
	default:
		more = false // the position cannot move; history.go errors with "did not move backwards"
	}
	pos.End = !more
	return got, pos, nil
}

// threadPager loads further pages of a sync group's thread list.
type threadPager interface {
	// Frontier returns the last activity timestamp of the oldest thread syncGroup's list has reached
	// and whether the list has more pages.
	Frontier(syncGroup int64) (minActivityTS int64, more bool)
	// FetchThreadPage hands the next page of syncGroup to handle (if there is one) and reports whether
	// the group has more pages.
	FetchThreadPage(ctx context.Context, syncGroup int64, handle func(*table.LSTable)) (bool, error)
}

type realPager struct{ rc *realMetaClient }

func (p realPager) Frontier(group int64) (int64, bool) {
	keyStore := p.rc.GetSyncGroupKeyStore(group)
	if keyStore == nil {
		return 0, false
	}
	return keyStore.MinLastActivityTimestampMs, keyStore.HasMoreBefore
}

// FetchThreadPage follows threadbackfill.go: FetchMoreThreads, then wait for the key store to move. A
// page that leaves the minimum thread key unchanged ends the group, as there.
func (p realPager) FetchThreadPage(ctx context.Context, group int64, handle func(*table.LSTable)) (bool, error) {
	keyStore := p.rc.GetSyncGroupKeyStore(group)
	if keyStore == nil || !keyStore.HasMoreBefore {
		return false, nil
	}
	prevMin := keyStore.MinThreadKey
	gen := p.rc.ThreadRangeGeneration(group)
	_, tbl, err := p.rc.FetchMoreThreads(ctx, group)
	if err != nil {
		return true, err
	} else if tbl == nil {
		return false, nil
	}
	handle(tbl)
	wctx, cancel := context.WithTimeout(ctx, threadRangeTimeout)
	err = p.rc.WaitForThreadRange(wctx, group, gen)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, errHistoryTimeout
	}
	return keyStore.HasMoreBefore && keyStore.MinThreadKey != prevMin, nil
}

// convCursor lists the sync groups whose thread list still has pages: 1 (normal threads), then 95
// (E2EE threads). The position inside a group is kept by messagix.
type convCursor struct {
	Groups []int64 `json:"g"`
}

var threadListGroups = []int64{1, e2eeSyncGroup}

// FetchConversations loads the next page of the thread list and emits conversation_upsert for the 1-1
// conversations in it (groups stay hidden), then response{ok, data:{has_more, cursor}}. Of the normal
// (1) and E2EE (95) lists, the one that reaches the more recent activity is paged first, so
// conversations arrive newest first across both instead of one list after the other.
func (b *MetaBackend) FetchConversations(ctx context.Context, args FetchConversationsArgs, emit func(Event)) error {
	data, err := b.fetchConversations(ctx, args, emit)
	if err != nil {
		return err
	}
	return setResponseData(ctx, data)
}

func (b *MetaBackend) fetchConversations(cmdCtx context.Context, args FetchConversationsArgs, emit func(Event)) (json.RawMessage, error) {
	cur := convCursor{Groups: threadListGroups}
	if args.Cursor != nil && *args.Cursor != "" {
		cur = convCursor{}
		if err := decodeCursor(args.Cursor, &cur); err != nil {
			return nil, err
		}
		for _, g := range cur.Groups {
			if g != 1 && g != e2eeSyncGroup {
				return nil, &Error{Code: "bad_request", Message: "invalid cursor"}
			}
		}
	}
	s, err := b.liveSession()
	if err != nil {
		return nil, err
	}
	pager := b.pagerOverride
	if pager == nil {
		b.mu.Lock()
		rc, ok := b.client.(*realMetaClient)
		b.mu.Unlock()
		if !ok {
			return nil, &Error{Code: "disconnected", Message: "no active session"}
		}
		pager = realPager{rc}
	}
	ctx, cancel := withSession(cmdCtx, s.ctx)
	defer cancel()
	b.threadPaging.Lock()
	defer b.threadPaging.Unlock()
	groups := cur.Groups
	for len(groups) > 0 {
		next, newest := -1, int64(0)
		for i := 0; i < len(groups); i++ {
			ts, more := pager.Frontier(groups[i])
			if !more {
				groups = append(groups[:i:i], groups[i+1:]...)
				i--
			} else if next < 0 || ts > newest {
				next, newest = i, ts
			}
		}
		if next < 0 {
			break
		}
		if err = b.waitPagingSlot(ctx, "threads"); err != nil {
			if s.ctx.Err() != nil {
				return nil, &Error{Code: "disconnected", Message: "session ended"}
			}
			return nil, err
		}
		fetched := false
		group := groups[next]
		more, err := pager.FetchThreadPage(ctx, group, func(tbl *table.LSTable) {
			fetched = true
			b.applyFetchedTable(s, tbl, emit)
		})
		if err != nil {
			if s.ctx.Err() != nil {
				return nil, &Error{Code: "disconnected", Message: "session ended"}
			}
			var e *Error
			if errors.As(err, &e) {
				return nil, err
			}
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, &Error{Code: reasonCode(err), Message: "failed to fetch conversations"}
		}
		if !more {
			groups = append(groups[:next:next], groups[next+1:]...)
		}
		if fetched {
			break
		}
	}
	return json.Marshal(struct {
		HasMore bool   `json:"has_more"`
		Cursor  string `json:"cursor"`
	}{len(groups) > 0, encodeCursor(convCursor{Groups: groups})})
}

func isNeedsRestore(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "needs_restore"
}
