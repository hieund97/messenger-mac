package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	DataDir  string `json:"data_dir"`
	Backend  string `json:"backend"`
	LogLevel string `json:"log_level"`
}
type Backend interface {
	Login(context.Context, LoginArgs, func(Event)) error
	RestoreHistory(context.Context, RestoreHistoryArgs, func(Event)) error
	SkipHistoryRestore(context.Context, SkipHistoryRestoreArgs, func(Event)) error
	Logout(context.Context, LogoutArgs, func(Event)) error
	Reconnect(context.Context, ReconnectArgs, func(Event)) error
	ListConversations(context.Context, ListConversationsArgs, func(Event)) error
	FetchMessages(context.Context, FetchMessagesArgs, func(Event)) error
	SendText(context.Context, SendTextArgs, func(Event)) error
	SendMedia(context.Context, SendMediaArgs, func(Event)) error
	MarkRead(context.Context, MarkReadArgs, func(Event)) error
	DownloadMedia(context.Context, DownloadMediaArgs, func(Event)) error
}

var nextRequestID atomic.Int64

type Engine struct {
	backend Backend
	emit    func(Event)
	emitMu  sync.Mutex
	stopped bool // guarded by emitMu
	mu      sync.Mutex
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
}

func New(cfg Config, emit func(Event)) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{emit: emit, ctx: ctx, cancel: cancel}
	if cfg.Backend == "fake" {
		e.backend = FakeBackend{}
	} else if cfg.Backend == "" || cfg.Backend == "meta" {
		e.backend = NewMetaBackend(cfg)
	}
	return e
}
func (e *Engine) send(ev Event) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	defer func() { _ = recover() }()
	if !e.stopped && e.emit != nil {
		e.emit(ev)
	}
}
func (e *Engine) Call(raw []byte) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return -1
	}
	id := nextRequestID.Add(1)
	input := append([]byte(nil), raw...)
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.run(id, input) }()
	return id
}
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	if backend, ok := e.backend.(*MetaBackend); ok {
		_ = backend.Logout(context.Background(), LogoutArgs{}, nil)
	}
	e.emitMu.Lock()
	e.stopped = true
	e.emitMu.Unlock()
}

// responseSentKey carries a channel that is closed once the command's response was emitted.
type responseSentKey struct{}

// responseSent returns the channel closed after the response of the command whose context is ctx was
// emitted, or nil outside a command. Events that must follow the response (message_send_result) wait
// on it.
func responseSent(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	ch, _ := ctx.Value(responseSentKey{}).(chan struct{})
	if ch == nil {
		return nil
	}
	return ch
}

func (e *Engine) run(id int64, raw []byte) {
	response := Response{OK: true}
	responded := make(chan struct{})
	ctx := context.WithValue(e.ctx, responseSentKey{}, responded)
	defer func() {
		if recover() != nil {
			response = Response{Error: &Error{Code: "internal", Message: "backend panic"}}
		}
		e.send(Event{Type: EventResponse, RequestID: &id, Data: response})
		close(responded)
	}()
	fail := func(code, message string) { response = Response{Error: &Error{Code: code, Message: message}} }
	var req Request
	if json.Unmarshal(raw, &req) != nil || req.Cmd == "" || len(req.Args) == 0 || req.Args[0] != '{' {
		fail("bad_request", "invalid request")
		return
	}
	if e.backend == nil {
		fail("internal", "backend unavailable")
		return
	}
	emit := func(ev Event) { ev.RequestID = &id; e.send(ev) }
	var (
		call func() error
		err  error
	)
	switch req.Cmd {
	case CmdLogin:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.Login)
	case CmdRestoreHistory:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.RestoreHistory)
	case CmdSkipHistoryRestore:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.SkipHistoryRestore)
	case CmdLogout:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.Logout)
	case CmdReconnect:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.Reconnect)
	case CmdListConversations:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.ListConversations)
	case CmdFetchMessages:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.FetchMessages)
	case CmdSendText:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.SendText)
	case CmdSendMedia:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.SendMedia)
	case CmdMarkRead:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.MarkRead)
	case CmdDownloadMedia:
		call, err = decodeArgs(req.Args, ctx, emit, e.backend.DownloadMedia)
	default:
		fail("unknown_cmd", "unknown command")
		return
	}
	if err != nil {
		fail("bad_request", "invalid arguments")
		return
	}
	if err := call(); err != nil {
		var backendError *Error
		if errors.As(err, &backendError) && backendError.Code != "" {
			response = Response{Error: backendError}
		} else {
			fail("internal", err.Error())
		}
	}
}

// Decode arguments before invoking the backend so backend errors retain their codes.
func decodeArgs[T any](raw json.RawMessage, ctx context.Context, emit func(Event), method func(context.Context, T, func(Event)) error) (func() error, error) {
	var args T
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return func() error { return method(ctx, args, emit) }, nil
}
