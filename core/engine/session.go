package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
)

type metaClient interface {
	LoadInbox(context.Context) error
	Connect(context.Context) error
	Disconnect()
	IsConnected() bool
	SetEventHandler(func(context.Context, any))
}
type realMetaClient struct{ *messagix.Client }

func (c *realMetaClient) LoadInbox(ctx context.Context) error {
	_, _, err := c.LoadMessagesPage(ctx)
	return err
}
func (c *realMetaClient) SetEventHandler(f func(context.Context, any)) { c.Client.SetEventHandler(f) }

type MetaBackend struct {
	cfg                Config
	op                 sync.Mutex
	mu                 sync.Mutex
	client             metaClient
	emit               func(Event)
	auth               bool
	cookies            *cookies.Cookies
	snapshot           map[cookies.MetaCookieName]string
	cancel             context.CancelFunc
	logFile            *os.File
	lastFatalReconnect time.Time
	newClient          func(*cookies.Cookies, zerolog.Logger) metaClient
}

func NewMetaBackend(cfg Config) *MetaBackend { return &MetaBackend{cfg: cfg} }
func (b *MetaBackend) Login(ctx context.Context, args LoginArgs, emit func(Event)) error {
	b.op.Lock()
	defer b.op.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p := types.PlatformFromString(args.Platform)
	if p != types.Messenger && p != types.Facebook {
		return &Error{Code: "bad_request", Message: "invalid platform"}
	}
	values := map[cookies.MetaCookieName]string{}
	ranks := map[string]int{}
	for _, c := range args.Cookies {
		rank := 0
		switch c.Domain {
		case ".messenger.com", "messenger.com":
			rank = 1
			if p == types.Messenger {
				rank = 2
			}
		case ".facebook.com", "facebook.com":
			rank = 1
			if p == types.Facebook {
				rank = 2
			}
		case "":
			rank = 1
		default:
			continue
		}
		if rank >= ranks[c.Name] {
			values[cookies.MetaCookieName(c.Name)] = c.Value
			ranks[c.Name] = rank
		}
	}
	c := &cookies.Cookies{Platform: p}
	c.UpdateValues(values)
	if len(c.GetMissingCookieNames()) > 0 {
		return &Error{Code: "missing_cookies", Message: "required cookies are missing"}
	}
	if c.GetUserID() <= 0 {
		return &Error{Code: "bad_request", Message: "invalid c_user"}
	}
	b.stop()
	b.mu.Lock()
	b.cookies = c
	b.snapshot = c.GetAll()
	b.emit = emit
	b.auth = false
	b.lastFatalReconnect = time.Time{}
	b.mu.Unlock()
	return b.startSession(ctx, c, p)
}

// startSession is called with op held. The cookies survive a failed session.
func (b *MetaBackend) startSession(ctx context.Context, c *cookies.Cookies, platform types.Platform) error {
	var file *os.File
	log := zerolog.Nop()
	if b.cfg.DataDir != "" {
		dir := filepath.Join(b.cfg.DataDir, strconv.FormatInt(c.GetUserID(), 10))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		var err error
		file, err = os.OpenFile(filepath.Join(dir, "core.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		log = zerolog.New(file).Level(zerolog.ErrorLevel)
	}
	_ = platform // The platform is already retained in c.
	var client metaClient
	if b.newClient != nil {
		client = b.newClient(c, log)
	} else {
		client = &realMetaClient{messagix.NewClient(c, log, &messagix.Config{})}
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.client = client
	b.cancel = cancel
	b.logFile = file
	b.mu.Unlock()
	client.SetEventHandler(func(_ context.Context, event any) { b.handleEvent(client, event) })
	if err := client.LoadInbox(sessionCtx); err != nil {
		b.failure(client, err)
		return &Error{Code: reasonCode(err), Message: "failed to load inbox"}
	}
	b.checkCookies(client)
	b.state(client, "connecting", "")
	if err := client.Connect(sessionCtx); err != nil {
		b.failure(client, err)
		return &Error{Code: reasonCode(err), Message: "failed to connect"}
	}
	go b.watchCookies(sessionCtx, client)
	return nil
}
func reasonCode(err error) string { _, code := classifyError(err); return code }
func (b *MetaBackend) state(client metaClient, state, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != client || b.auth {
		return
	}
	var r *string
	if reason != "" {
		r = &reason
	}
	if b.emit != nil {
		b.emit(Event{Type: EventConnectionState, Data: ConnectionState{State: state, Reason: r}})
	}
}
func (b *MetaBackend) failure(client metaClient, err error) {
	state, reason := classifyError(err)
	b.mu.Lock()
	if b.client != client || b.auth {
		b.mu.Unlock()
		return
	}
	if state == "auth_required" {
		b.auth = true
		if b.cancel != nil {
			b.cancel()
		}
	}
	emit := b.emit
	if emit != nil {
		emit(Event{Type: EventConnectionState, Data: ConnectionState{State: state, Reason: &reason}})
	}
	b.mu.Unlock()
	// The handler runs on messagix's connection loop, so Disconnect must not wait on itself.
	if state == "auth_required" {
		go client.Disconnect()
	}
}
func (b *MetaBackend) handleEvent(client metaClient, event any) {
	switch e := event.(type) {
	case *messagix.ConnectedEvent, *messagix.ReconnectedEvent:
		b.checkCookies(client)
		b.state(client, "connected", "")
	case *messagix.TransientDisconnectEvent:
		b.handleDisconnect(client, e.Err)
	case *messagix.PermanentErrorEvent:
		b.handleDisconnect(client, e.Err)
	}
}

func (b *MetaBackend) handleDisconnect(client metaClient, err error) {
	b.failure(client, err)
	if errors.Is(err, messagix.ErrFatalSocketSyncError) {
		b.mu.Lock()
		allowed := b.client == client && !b.auth && time.Since(b.lastFatalReconnect) >= time.Minute
		if allowed {
			b.lastFatalReconnect = time.Now()
		}
		b.mu.Unlock()
		if allowed {
			go b.reconnectAfterFatal(client)
		}
	}
}
func (b *MetaBackend) checkCookies(client metaClient) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != client || b.auth || b.cookies == nil {
		return
	}
	if len(b.cookies.GetMissingCookieNames()) > 0 || !b.cookies.IsLoggedIn() {
		return
	}
	current := b.cookies.GetAll()
	if reflect.DeepEqual(current, b.snapshot) {
		return
	}
	b.snapshot = current
	domain := ".facebook.com"
	if b.cookies.Platform == types.Messenger {
		domain = ".messenger.com"
	}
	names := make([]string, 0, len(current))
	for name := range current {
		names = append(names, string(name))
	}
	sort.Strings(names)
	list := make([]Cookie, 0, len(names))
	for _, name := range names {
		list = append(list, Cookie{Name: name, Value: current[cookies.MetaCookieName(name)], Domain: domain})
	}
	// messagix retains only name/value; domain is inferred and expiry is unavailable.
	if b.emit != nil {
		b.emit(Event{Type: EventCookiesUpdated, Data: CookiesUpdated{Cookies: list}})
	}
}
func (b *MetaBackend) watchCookies(ctx context.Context, client metaClient) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.checkCookies(client)
		}
	}
}
func (b *MetaBackend) stop() {
	b.mu.Lock()
	c, cancel, file := b.client, b.cancel, b.logFile
	b.client = nil
	b.cookies = nil
	b.snapshot = nil
	b.emit = nil
	b.cancel = nil
	b.logFile = nil
	b.auth = false
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if c != nil {
		go c.Disconnect()
	}
	if file != nil {
		_ = file.Close()
	}
}
func (b *MetaBackend) Logout(_ context.Context, _ LogoutArgs, _ func(Event)) error {
	b.mu.Lock()
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	b.op.Lock()
	defer b.op.Unlock()
	b.stop()
	return nil
}
func (b *MetaBackend) Reconnect(ctx context.Context, _ ReconnectArgs, _ func(Event)) error {
	b.op.Lock()
	defer b.op.Unlock()
	return b.reconnect(ctx, nil)
}

func (b *MetaBackend) reconnectAfterFatal(client metaClient) {
	b.op.Lock()
	defer b.op.Unlock()
	_ = b.reconnect(context.Background(), client)
}

// expected restricts an automatic reconnect to the session that emitted the error.
func (b *MetaBackend) reconnect(ctx context.Context, expected metaClient) error {
	b.mu.Lock()
	client, c, auth := b.client, b.cookies, b.auth
	if expected != nil && client != expected {
		b.mu.Unlock()
		return nil
	}
	if auth {
		b.mu.Unlock()
		return &Error{Code: "auth_required", Message: "login required"}
	}
	if c == nil {
		b.mu.Unlock()
		return &Error{Code: "disconnected", Message: "no active session"}
	}
	if expected == nil && client != nil && client.IsConnected() {
		b.mu.Unlock()
		return nil
	}
	cancel, file := b.cancel, b.logFile
	b.client, b.cancel, b.logFile = nil, nil, nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		go client.Disconnect()
	}
	if file != nil {
		_ = file.Close()
	}
	return b.startSession(ctx, c, c.Platform)
}
func notImplemented() error { return &Error{Code: "not_implemented", Message: "not implemented"} }
func (*MetaBackend) RestoreHistory(context.Context, RestoreHistoryArgs, func(Event)) error {
	return notImplemented()
}
func (*MetaBackend) SkipHistoryRestore(context.Context, SkipHistoryRestoreArgs, func(Event)) error {
	return notImplemented()
}
func (*MetaBackend) ListConversations(context.Context, ListConversationsArgs, func(Event)) error {
	return notImplemented()
}
func (*MetaBackend) FetchMessages(context.Context, FetchMessagesArgs, func(Event)) error {
	return notImplemented()
}
func (*MetaBackend) SendText(context.Context, SendTextArgs, func(Event)) error {
	return notImplemented()
}
func (*MetaBackend) SendMedia(context.Context, SendMediaArgs, func(Event)) error {
	return notImplemented()
}
func (*MetaBackend) MarkRead(context.Context, MarkReadArgs, func(Event)) error {
	return notImplemented()
}
func (*MetaBackend) DownloadMedia(context.Context, DownloadMediaArgs, func(Event)) error {
	return notImplemented()
}
