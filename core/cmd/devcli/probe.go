package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"macmess/core/engine"
)

// prober runs commands one at a time and prints only counts, timestamps and flags: no names, text
// or secrets. It is the -fetch-conversations / -fetch-messages / -list-conversations mode.
type prober struct {
	e       *engine.Engine
	mu      sync.Mutex
	waiting map[int64]chan engine.Response
	convs   map[string]*probeConv
	// per request counters, keyed by request ID
	upserts map[int64]*probeCount
}

type probeConv struct {
	restored     bool
	lastActivity int64
	normal, e2ee int // messages seen per thread kind
}

type probeCount struct {
	newConvs   int
	convEvents int
	msgs       map[string][]engine.Message // per conversation
}

func newProber() *prober {
	return &prober{waiting: map[int64]chan engine.Response{}, convs: map[string]*probeConv{}, upserts: map[int64]*probeCount{}}
}

func (p *prober) count(id int64) *probeCount {
	c := p.upserts[id]
	if c == nil {
		c = &probeCount{msgs: map[string][]engine.Message{}}
		p.upserts[id] = c
	}
	return c
}

// observe is called for every event; it returns true when the event was consumed (not printed).
func (p *prober) observe(ev engine.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rid := int64(0)
	if ev.RequestID != nil {
		rid = *ev.RequestID
	}
	switch d := ev.Data.(type) {
	case engine.Response:
		if ch, ok := p.waiting[rid]; ok {
			delete(p.waiting, rid)
			ch <- d
			return true
		}
	case engine.Conversation:
		c, ok := p.convs[d.ID]
		if !ok {
			c = &probeConv{}
			p.convs[d.ID] = c
			p.count(rid).newConvs++
		}
		c.restored = d.HistoryRestored
		c.lastActivity = d.LastActivityTS
		p.count(rid).convEvents++
		return true
	case engine.Message:
		c := p.convs[d.ConversationID]
		if c != nil {
			if d.E2EE {
				c.e2ee++
			} else {
				c.normal++
			}
		}
		p.count(rid).msgs[d.ConversationID] = append(p.count(rid).msgs[d.ConversationID], d)
		return true
	}
	return false
}

func (p *prober) call(cmd string, args any, timeout time.Duration) (engine.Response, int64, time.Duration) {
	raw, _ := json.Marshal(map[string]any{"cmd": cmd, "args": args})
	ch := make(chan engine.Response, 1)
	start := time.Now()
	// The request ID is allocated inside Call; register under the lock so the response cannot be
	// missed: observe takes the same lock.
	p.mu.Lock()
	id := p.e.Call(raw)
	p.waiting[id] = ch
	p.mu.Unlock()
	select {
	case r := <-ch:
		return r, id, time.Since(start)
	case <-time.After(timeout):
		return engine.Response{Error: &engine.Error{Code: "devcli_timeout"}}, id, time.Since(start)
	}
}

func (p *prober) snapshot() (total, restored int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.convs {
		total++
		if c.restored {
			restored++
		}
	}
	return
}

func respErr(r engine.Response) string {
	if r.Error != nil {
		return r.Error.Code
	}
	return ""
}

// cursorShape describes a fetch cursor without IDs: the groups of a conversation cursor, or the
// ts/end/strict flags of a message cursor.
func cursorShape(s string) string {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "?"
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return "?"
	}
	var parts []string
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch x := v[k].(type) {
		case map[string]any:
			var sub []string
			for _, f := range []string{"ts", "end", "strict"} {
				if val, ok := x[f]; ok {
					sub = append(sub, fmt.Sprintf("%s=%v", f, val))
				}
			}
			if ids, ok := x["ids"].([]any); ok {
				sub = append(sub, fmt.Sprintf("ids=%d", len(ids)))
			}
			parts = append(parts, k+"{"+strings.Join(sub, ",")+"}")
		default:
			parts = append(parts, fmt.Sprintf("%s=%v", k, x))
		}
	}
	return strings.Join(parts, " ")
}

func (p *prober) fetchConversations(pages int) {
	var cursor *string
	for page := 1; page <= pages; page++ {
		before, _ := p.snapshot()
		r, id, took := p.call(engine.CmdFetchConversations, engine.FetchConversationsArgs{Cursor: cursor}, 3*time.Minute)
		after, restored := p.snapshot()
		var data struct {
			HasMore bool   `json:"has_more"`
			Cursor  string `json:"cursor"`
		}
		_ = json.Unmarshal(r.Data, &data)
		p.mu.Lock()
		c := p.count(id)
		events := c.convEvents
		p.mu.Unlock()
		fmt.Fprintf(os.Stderr, "fetch_conversations page=%d ok=%v err=%s took=%s new_convs=%d conv_events=%d total=%d restored=%d has_more=%v cursor=[%s]\n",
			page, r.OK, respErr(r), took.Round(time.Millisecond), after-before, events, after, restored, data.HasMore, cursorShape(data.Cursor))
		if !r.OK || !data.HasMore {
			return
		}
		cur := data.Cursor
		cursor = &cur
	}
}

func (p *prober) listConversations() {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.convs))
	for id := range p.convs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return p.convs[ids[i]].lastActivity > p.convs[ids[j]].lastActivity })
	for i, id := range ids {
		c := p.convs[id]
		fmt.Fprintf(os.Stderr, "conv #%d id=%s last_activity=%s restored=%v msgs_normal=%d msgs_e2ee=%d\n",
			i+1, id, time.UnixMilli(c.lastActivity).Format(time.DateTime), c.restored, c.normal, c.e2ee)
	}
}

func (p *prober) fetchMessages(conv string, pages, limit int) {
	var cursor *string
	total := 0
	for page := 1; page <= pages; page++ {
		r, id, took := p.call(engine.CmdFetchMessages, engine.FetchMessagesArgs{ConversationID: conv, Cursor: cursor, Limit: limit}, 5*time.Minute)
		var data struct {
			HasMore      bool   `json:"has_more"`
			Cursor       string `json:"cursor"`
			NeedsRestore bool   `json:"needs_restore"`
		}
		_ = json.Unmarshal(r.Data, &data)
		p.mu.Lock()
		msgs := p.count(id).msgs[conv]
		p.mu.Unlock()
		e2ee, oldest, newest := 0, int64(0), int64(0)
		for _, m := range msgs {
			if m.E2EE {
				e2ee++
			}
			if oldest == 0 || m.TS < oldest {
				oldest = m.TS
			}
			newest = max(newest, m.TS)
		}
		total += len(msgs)
		span := "-"
		if len(msgs) > 0 {
			span = time.UnixMilli(oldest).Format(time.DateTime) + " .. " + time.UnixMilli(newest).Format(time.DateTime)
		}
		fmt.Fprintf(os.Stderr, "fetch_messages %s page=%d ok=%v err=%s took=%s msgs=%d e2ee=%d total=%d span=[%s] has_more=%v needs_restore=%v cursor=[%s]\n",
			conv, page, r.OK, respErr(r), took.Round(time.Millisecond), len(msgs), e2ee, total, span, data.HasMore, data.NeedsRestore, cursorShape(data.Cursor))
		if !r.OK || !data.HasMore {
			return
		}
		cur := data.Cursor
		cursor = &cur
	}
}
