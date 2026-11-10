package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
)

type metaClient interface {
	LoadInbox(context.Context) error
	Connect(context.Context) error
	Disconnect()
	IsConnected() bool
	SetEventHandler(func(context.Context, any))
}
type realMetaClient struct {
	*messagix.Client
	initial atomic.Pointer[table.LSTable] // inbox page table, handled once the socket is connected
}

func (c *realMetaClient) LoadInbox(ctx context.Context) error {
	_, tbl, err := c.LoadMessagesPage(ctx)
	if err == nil && tbl != nil {
		c.initial.Store(tbl)
	}
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
	// Per login: conversations survive reconnects. Per session: e2ee, ctx and connected.
	index     *ThreadIndex
	e2ee      *e2eeConn
	ctx       context.Context
	connected bool
	// E2EE-triggered full reconnects back off 1, 2, 5, 10 minutes; reset once E2EE connects.
	e2eeFullReconnects int
	lastE2EEReconnect  time.Time
	icdcFailures       int
	// Sending, read receipts and media (send.go, media.go). Tests set the overrides; nil means the
	// live session's messagix and whatsmeow clients.
	lsOverride  lsOps
	waOverride  waOps
	sendTimeout time.Duration // 0 means sendConfirmTimeout
	mediaHTTP   *http.Client  // nil means sharedMediaClient
	waits       confirmWaits
	sends       sendQueue
	refreshes   mediaRefreshes
	// onE2EESent, when set, is called after each successful E2EE send (see E2EESentHook).
	onE2EESent E2EESentHook
	// History (history.go, securehistory.go). history is reset per login; the overrides are for tests.
	history         *historyState
	historyOverride historyOps
	pagerOverride   threadPager
	pagingDelay     time.Duration // 0 means defaultPagingDelay
	pagingMu        sync.Mutex
	lastPage        map[string]time.Time // spacing of history batches and thread list pages
	fetching        map[string]bool      // conversations with a fetch_messages running
	threadPaging    sync.Mutex           // one thread list page request at a time
}

var e2eeFullReconnectBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

func NewMetaBackend(cfg Config) *MetaBackend {
	b := &MetaBackend{cfg: cfg, history: &historyState{}}
	b.onE2EESent = b.queueHistoryUpload
	return b
}
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
	b.index = NewThreadIndex(c.GetUserID())
	// With the device saved from an earlier restore, E2EE history is readable without the PIN.
	device := deviceFromSecrets(args.Secrets, c.GetUserID())
	b.history.reset(device)
	b.index.SetHistoryRestored(device != nil, nil)
	b.e2eeFullReconnects, b.lastE2EEReconnect, b.icdcFailures = 0, time.Time{}, 0
	b.mu.Unlock()
	return b.startSession(ctx, c, p)
}

// startSession is called with op held. The cookies survive a failed session.
func (b *MetaBackend) startSession(ctx context.Context, c *cookies.Cookies, platform types.Platform) error {
	var file *os.File
	log := zerolog.Nop()
	dir := ""
	if b.cfg.DataDir != "" {
		dir = filepath.Join(b.cfg.DataDir, strconv.FormatInt(c.GetUserID(), 10))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		var err error
		file, err = os.OpenFile(filepath.Join(dir, "core.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		// messagix logs request URLs (with fb_dtsg in the query) on errors; never let them reach the file.
		log = zerolog.New(redactingWriter{file}).Level(zerolog.ErrorLevel)
	}
	_ = platform // The platform is already retained in c.
	var client metaClient
	if b.newClient != nil {
		client = b.newClient(c, log)
	} else {
		client = &realMetaClient{Client: messagix.NewClient(c, log, &messagix.Config{})}
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	index := b.index
	var e2ee *e2eeConn
	if rc, ok := client.(*realMetaClient); ok && dir != "" && index != nil {
		// whatsmeow logs through the messagix logger (PrepareE2EEClient), i.e. into the same core.log.
		e2ee = newE2EEConn(sessionCtx, b, client, rc.Client, index, filepath.Join(dir, "e2ee.db"), log)
	}
	b.client = client
	b.cancel = cancel
	b.logFile = file
	b.e2ee = e2ee
	b.ctx = sessionCtx
	b.connected = false
	b.mu.Unlock()
	if index != nil {
		index.setLogger(log)
	}
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
	var e2ee *e2eeConn
	if state == "auth_required" {
		b.auth = true
		if b.cancel != nil {
			b.cancel()
		}
		e2ee, b.e2ee = b.e2ee, nil
	}
	emit := b.emit
	if emit != nil {
		emit(Event{Type: EventConnectionState, Data: ConnectionState{State: state, Reason: &reason}})
	}
	b.mu.Unlock()
	// The handler runs on messagix's (or whatsmeow's) event loop, so Disconnect must not wait on itself.
	if state == "auth_required" {
		go client.Disconnect()
		if e2ee != nil {
			go e2ee.close()
		}
	}
}
func (b *MetaBackend) handleEvent(client metaClient, event any) {
	switch e := event.(type) {
	case *messagix.ConnectedEvent, *messagix.ReconnectedEvent:
		b.checkCookies(client)
		b.state(client, "connected", "")
		b.onConnected(client)
	case *table.LSTable:
		b.handleTable(client, e)
		// handlemeta.go:248: sync group cursors advance only after the table was handled.
		if p, ok := client.(interface{ PostHandlePublishResponse(*table.LSTable) }); ok && b.sink(client) != nil {
			p.PostHandlePublishResponse(e)
		}
	case *messagix.TransientDisconnectEvent:
		b.handleDisconnect(client, e.Err)
	case *messagix.PermanentErrorEvent:
		b.handleDisconnect(client, e.Err)
	}
}

func (b *MetaBackend) handleDisconnect(client metaClient, err error) {
	b.failure(client, err)
	if errors.Is(err, messagix.ErrFatalSocketSyncError) {
		b.scheduleFullReconnect(client)
	}
}

// scheduleFullReconnect starts a new session for client at most once per minute. It reports whether
// a reconnect was started.
func (b *MetaBackend) scheduleFullReconnect(client metaClient) bool {
	b.mu.Lock()
	allowed := b.client == client && !b.auth && time.Since(b.lastFatalReconnect) >= time.Minute
	if allowed {
		b.lastFatalReconnect = time.Now()
	}
	b.mu.Unlock()
	if allowed {
		go b.reconnectAfterFatal(client)
	}
	return allowed
}

// scheduleE2EEFullReconnect is the full reconnect E2EE asks for (CAT refresh, ICDC 401, WA 400).
// Consecutive ones are spaced by e2eeFullReconnectBackoff; false means the caller retries E2EE only.
func (b *MetaBackend) scheduleE2EEFullReconnect(client metaClient) bool {
	b.mu.Lock()
	allowed := b.client == client && !b.auth
	if allowed && b.e2eeFullReconnects > 0 {
		wait := e2eeFullReconnectBackoff[min(b.e2eeFullReconnects-1, len(e2eeFullReconnectBackoff)-1)]
		allowed = time.Since(b.lastE2EEReconnect) >= wait
	}
	if allowed {
		b.e2eeFullReconnects++
		b.lastE2EEReconnect = time.Now()
	}
	b.mu.Unlock()
	if allowed {
		go b.reconnectAfterFatal(client)
	}
	return allowed
}

func (b *MetaBackend) noteICDCFailure() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.icdcFailures++
	return b.icdcFailures
}

func (b *MetaBackend) e2eeHealthy() {
	b.mu.Lock()
	b.e2eeFullReconnects, b.icdcFailures = 0, 0
	b.mu.Unlock()
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
	b.history.reset(nil)
	b.mu.Lock()
	c, cancel, file, e2ee := b.client, b.cancel, b.logFile, b.e2ee
	b.client = nil
	b.cookies = nil
	b.snapshot = nil
	b.emit = nil
	b.cancel = nil
	b.logFile = nil
	b.auth = false
	b.index = nil
	b.e2ee = nil
	b.ctx = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if c != nil {
		go c.Disconnect()
	}
	if e2ee != nil {
		e2ee.closeAsync()
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
	cancel, file, e2ee := b.cancel, b.logFile, b.e2ee
	b.client, b.cancel, b.logFile, b.e2ee, b.ctx = nil, nil, nil, nil, nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		go client.Disconnect()
	}
	if e2ee != nil {
		e2ee.closeAsync()
	}
	if file != nil {
		_ = file.Close()
	}
	return b.startSession(ctx, c, c.Platform)
}

var (
	// logURLQuery matches the query of an http(s) URL inside a log line. It stops at whitespace, a
	// quote or a backslash (a JSON-escaped quote in an error string).
	logURLQuery = regexp.MustCompile(`(https?://[^\s"'\\?#]+)\?[^\s"'\\#]*`)
	// logSecretParam matches token parameters outside URLs: form bodies (fb_dtsg=...) and JSON fields
	// ("fb_dtsg":"..." or the escaped form inside an error string).
	logSecretParam = regexp.MustCompile(`(?i)\b((?:fb_dtsg|lsd|jazoest|access_token)(?:=|\\?"\s*:\s*\\?"))[^&\s"\\,}]*`)
)

// redactLogLine removes tokens and URL queries from one log line.
func redactLogLine(p []byte) []byte {
	p = logURLQuery.ReplaceAll(p, []byte("$1?REDACTED"))
	return logSecretParam.ReplaceAll(p, []byte("${1}REDACTED"))
}

// redactingWriter redacts every log line before writing it. zerolog writes one line per Write call.
type redactingWriter struct{ w io.Writer }

func (r redactingWriter) Write(p []byte) (int, error) {
	if _, err := r.w.Write(redactLogLine(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}
