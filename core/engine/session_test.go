package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/loginerrors"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
	waEvents "go.mau.fi/whatsmeow/types/events"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClassify1357001IsAuthRequired(t *testing.T) {
	s, c := classifyError(types.ErrPleaseLogIn)
	if s != "auth_required" || c != "token_invalid" {
		t.Fatal(s, c)
	}
}
func TestClassify429IsRateLimited(t *testing.T) {
	s, c := classifyError(httpclient.ErrRateLimited)
	if s != "disconnected" || c != "rate_limited" {
		t.Fatal(s, c)
	}
}
func TestClassifyCheckpoint(t *testing.T) {
	for _, err := range []error{loginerrors.Checkpoint, httpclient.ErrCheckpointRequired, httpclient.RedirectedError{Type: httpclient.ErrChallengeRequired}} {
		s, c := classifyError(fmt.Errorf("load inbox: %w", err))
		if s != "auth_required" || c != "checkpoint" {
			t.Fatal(err, s, c)
		}
	}
	// Cookie xs sai: messagix trả lỗi redirect về trang đăng nhập (ErrTokenInvalidatedRedirect).
	s, c := classifyError(fmt.Errorf("load inbox: %w", httpclient.RedirectedError{Type: httpclient.ErrTokenInvalidatedRedirect}))
	if s != "auth_required" || c != "token_invalid" {
		t.Fatal(s, c)
	}
}
func TestAuthRequiredEmittedOnceAndNoReconnect(t *testing.T) {
	b := NewMetaBackend(Config{})
	var events []Event
	b.emit = func(e Event) { events = append(events, e) }
	b.client = &testMetaClient{}
	client := b.client.(*testMetaClient)
	for i := 0; i < 3; i++ {
		b.handleEvent(client, &messagix.PermanentErrorEvent{Err: types.ErrPleaseLogIn})
	}
	if len(events) != 1 || events[0].Data.(ConnectionState).State != "auth_required" {
		t.Fatal(events)
	}
	if err := b.Reconnect(context.Background(), ReconnectArgs{}, nil); err == nil || err.(*Error).Code != "auth_required" {
		t.Fatal(err)
	}
	if client.connects != 0 {
		t.Fatal(client.connects)
	}
}

type testMetaClient struct {
	connects  int
	connected bool
	loadErr   error
}

func (c *testMetaClient) LoadInbox(context.Context) error          { return c.loadErr }
func (c *testMetaClient) IsConnected() bool                        { return c.connected }
func (c *testMetaClient) Connect(context.Context) error            { c.connects++; return nil }
func (*testMetaClient) Disconnect()                                {}
func (*testMetaClient) SetEventHandler(func(context.Context, any)) {}
func TestMissingRequiredCookie(t *testing.T) {
	events := make(chan Event, 1)
	e := New(Config{Backend: "meta"}, func(event Event) { events <- event })
	defer e.Shutdown()
	e.Call([]byte(`{"cmd":"login","args":{"platform":"messenger","cookies":[{"name":"c_user","value":"1"},{"name":"datr","value":"a"}]}}`))
	event := <-events
	response, ok := event.Data.(Response)
	if !ok || response.OK || response.Error == nil || response.Error.Code != "missing_cookies" {
		t.Fatal(event)
	}
}

func validArgs(platform string) LoginArgs {
	return LoginArgs{Platform: platform, Cookies: []Cookie{{Name: "c_user", Value: "1"}, {Name: "xs", Value: "x"}, {Name: "datr", Value: "d"}}}
}
func TestReconnectWhenConnectedIsNoop(t *testing.T) {
	b := NewMetaBackend(Config{})
	c := &testMetaClient{connected: true}
	b.client = c
	b.cookies = &cookies.Cookies{Platform: types.Messenger}
	if err := b.Reconnect(context.Background(), ReconnectArgs{}, nil); err != nil || b.client != c || c.connects != 0 {
		t.Fatal(err)
	}
}
func TestReconnectAfterFailedInboxStartsFreshSession(t *testing.T) {
	b := NewMetaBackend(Config{})
	first := &testMetaClient{loadErr: errors.New("network")}
	second := &testMetaClient{}
	count := 0
	b.newClient = func(*cookies.Cookies, zerolog.Logger) metaClient {
		count++
		if count == 1 {
			return first
		}
		return second
	}
	if b.Login(context.Background(), validArgs("messenger"), nil) == nil {
		t.Fatal("expected load error")
	}
	if err := b.Reconnect(context.Background(), ReconnectArgs{}, nil); err != nil || count != 2 || first.connects != 0 || second.connects != 1 {
		t.Fatal(err, count)
	}
	b.stop()
}
func TestFatalSyncErrorTriggersSingleFullReconnectPerMinute(t *testing.T) {
	b := NewMetaBackend(Config{})
	created := make(chan *testMetaClient, 2)
	b.newClient = func(*cookies.Cookies, zerolog.Logger) metaClient { c := &testMetaClient{}; created <- c; return c }
	if err := b.Login(context.Background(), validArgs("messenger"), nil); err != nil {
		t.Fatal(err)
	}
	first := <-created
	b.handleEvent(first, &messagix.TransientDisconnectEvent{Err: fmt.Errorf("sync: %w", messagix.ErrFatalSocketSyncError)})
	var second *testMetaClient
	select {
	case second = <-created:
	case <-time.After(time.Second):
		t.Fatal("reconnect missing")
	}
	b.op.Lock()
	b.op.Unlock()
	b.handleEvent(second, &messagix.PermanentErrorEvent{Err: messagix.ErrFatalSocketSyncError})
	select {
	case <-created:
		t.Fatal("unexpected reconnect")
	case <-time.After(50 * time.Millisecond):
	}
	b.stop()
}
func TestCookiesUpdatedSkippedWhenXsMissing(t *testing.T) {
	b := NewMetaBackend(Config{})
	c := &testMetaClient{}
	b.client = c
	b.cookies = &cookies.Cookies{Platform: types.Messenger}
	b.cookies.UpdateValues(map[cookies.MetaCookieName]string{"c_user": "1", "datr": "d", "xs": "x"})
	b.snapshot = b.cookies.GetAll()
	count := 0
	b.emit = func(Event) { count++ }
	b.cookies.UpdateValues(map[cookies.MetaCookieName]string{"c_user": "1", "datr": "new"})
	b.checkCookies(c)
	if count != 0 || b.snapshot["datr"] != "d" {
		t.Fatal(count, b.snapshot)
	}
}
func TestNoEmitAfterStop(t *testing.T) {
	b := NewMetaBackend(Config{})
	c := &testMetaClient{}
	b.client = c
	var mu sync.Mutex
	count := 0
	b.emit = func(Event) { mu.Lock(); count++; mu.Unlock() }
	b.stop()
	b.handleEvent(c, &messagix.ConnectedEvent{})
	b.handleEvent(c, &messagix.PermanentErrorEvent{Err: types.ErrPleaseLogIn})
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatal(count)
	}
}
func TestPlatformPrefersOwnDomainCookies(t *testing.T) {
	for _, platform := range []string{"facebook", "messenger"} {
		t.Run(platform, func(t *testing.T) {
			b := NewMetaBackend(Config{})
			var got string
			b.newClient = func(c *cookies.Cookies, _ zerolog.Logger) metaClient {
				got = c.GetAll()["xs"]
				return &testMetaClient{}
			}
			args := validArgs(platform)
			args.Cookies = append(args.Cookies, Cookie{Name: "xs", Value: "fb", Domain: ".facebook.com"}, Cookie{Name: "xs", Value: "msg", Domain: ".messenger.com"})
			if err := b.Login(context.Background(), args, nil); err != nil {
				t.Fatal(err)
			}
			want := "fb"
			if platform == "messenger" {
				want = "msg"
			}
			if got != want {
				t.Fatal(got, want)
			}
			b.stop()
		})
	}
}
func TestTooManyRedirectsIsNetwork(t *testing.T) {
	s, c := classifyError(httpclient.ErrTooManyRedirects)
	if s != "disconnected" || c != "network" {
		t.Fatal(s, c)
	}
}

func TestE2EEFailuresNeverRequireAuth(t *testing.T) {
	b := NewMetaBackend(Config{})
	client := &testMetaClient{}
	var mu sync.Mutex
	var got []Event
	b.client = client
	b.emit = func(e Event) { mu.Lock(); got = append(got, e); mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newE2EEConn(ctx, b, client, nil, NewThreadIndex(1), "", zerolog.Nop())
	e.reset = true // the one re-registration of this session is already used
	defer e.close()
	for _, evt := range []any{&waEvents.LoggedOut{}, &waEvents.ClientOutdated{}, &waEvents.TemporaryBan{},
		&waEvents.ConnectFailure{Reason: waEvents.ConnectFailureNotFound}, &waEvents.StreamReplaced{}} {
		if !e.handle(nil, evt) {
			t.Fatalf("%T not acked", evt)
		}
	}
	mu.Lock()
	codes := []string{}
	for _, ev := range got {
		if ev.Type != EventError {
			t.Fatalf("unexpected event %+v", ev)
		}
		codes = append(codes, ev.Data.(ErrorEvent).Code)
	}
	mu.Unlock()
	if len(codes) != 2 || codes[0] != "e2ee_unavailable" || codes[1] != "e2ee_stream_replaced" {
		t.Fatal(codes)
	}
	if b.auth {
		t.Fatal("E2EE failure switched the session to auth_required")
	}
	if s, _ := classifyError(errors.New("e2ee device logged out")); s == "auth_required" {
		t.Fatal(s)
	}

	// With no live session a message is not acked, so the server delivers it again.
	b.mu.Lock()
	b.client = nil
	b.mu.Unlock()
	if e.handle(nil, &waEvents.FBMessage{}) {
		t.Fatal("message acked without a session")
	}
}

func TestCoreLogRedactsTokens(t *testing.T) {
	dir := t.TempDir()
	b := NewMetaBackend(Config{DataDir: dir})
	b.newClient = func(_ *cookies.Cookies, log zerolog.Logger) metaClient {
		// What messagix logs on a failed Mercury request (httpclient/http.go, mercury.go).
		urlErr := &url.Error{Op: "Post", URL: "https://www.facebook.com/ajax/mercury/upload.php?__a=1&fb_dtsg=SECRET1&jazoest=SECRET2", Err: errors.New("EOF")}
		log.Error().Err(urlErr).
			Str("url", "https://upload.messenger.com/ajax/mercury/upload.php?fb_dtsg=SECRET3&lsd=SECRET4").
			Str("body", "av=1&fb_dtsg=SECRET5&lsd=SECRET6&jazoest=SECRET7").
			Str("json", `{"access_token":"SECRET8","fb_dtsg": "SECRET9"}`).
			Msg("Failed to send Mercury request")
		return &testMetaClient{}
	}
	if err := b.Login(context.Background(), validArgs("messenger"), nil); err != nil {
		t.Fatal(err)
	}
	b.stop()
	data, err := os.ReadFile(filepath.Join(dir, "1", "core.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET") {
		t.Fatalf("token in log: %s", data)
	}
	if !strings.Contains(string(data), "upload.php?REDACTED") || !strings.Contains(string(data), "Failed to send Mercury request") {
		t.Fatalf("log = %s", data)
	}
}
