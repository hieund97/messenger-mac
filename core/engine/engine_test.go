package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func collect(t *testing.T, e *Engine, events <-chan Event, raw string) (int64, []Event) {
	t.Helper()
	id := e.Call([]byte(raw))
	var got []Event
	for {
		select {
		case event := <-events:
			got = append(got, event)
			if event.Type == EventResponse {
				return id, got
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for response")
		}
	}
}

func responseData(t *testing.T, event Event) map[string]any {
	t.Helper()
	b, err := json.Marshal(event.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUnknownCmdReturnsError(t *testing.T) {
	events := make(chan Event, 10)
	e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
	defer e.Shutdown()
	_, got := collect(t, e, events, `{"cmd":"unknown","args":{}}`)
	data := responseData(t, got[len(got)-1])
	if data["ok"] != false || data["error"].(map[string]any)["code"] != "unknown_cmd" {
		t.Fatalf("response: %#v", data)
	}
}

func TestRequestIDEchoed(t *testing.T) {
	events := make(chan Event, 10)
	e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
	defer e.Shutdown()
	id, got := collect(t, e, events, `{"request_id":999,"cmd":"logout","args":{}}`)
	if got[len(got)-1].RequestID == nil || *got[len(got)-1].RequestID != id {
		t.Fatalf("id %d, response %#v", id, got[len(got)-1])
	}
}

func TestFakeLoginEmitsConnectedThenConversations(t *testing.T) {
	events := make(chan Event, 10)
	e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
	defer e.Shutdown()
	_, got := collect(t, e, events, `{"cmd":"login","args":{"cookies":[],"platform":"messenger"}}`)
	if len(got) != 8 || got[0].Type != EventConnectionState || responseData(t, got[0])["state"] != "connected" || got[1].Type != EventConversationUpsert || got[2].Type != EventConversationUpsert ||
		got[6].Type != EventHistoryRestoreState || responseData(t, got[6])["state"] != "needed" {
		t.Fatalf("events: %#v", got)
	}
}

func TestMalformedJSONDoesNotPanic(t *testing.T) {
	events := make(chan Event, 10)
	e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
	defer e.Shutdown()
	_, got := collect(t, e, events, `{`)
	data := responseData(t, got[len(got)-1])
	if data["ok"] != false || data["error"].(map[string]any)["code"] != "bad_request" {
		t.Fatalf("response: %#v", data)
	}
}

// Override one command while keeping the rest of the fake backend contract.
type loginBackend struct {
	FakeBackend
	login func(context.Context, LoginArgs, func(Event)) error
}

func (b loginBackend) Login(ctx context.Context, args LoginArgs, emit func(Event)) error {
	return b.login(ctx, args, emit)
}

func TestPanicInBackendRecovered(t *testing.T) {
	events := make(chan Event, 10)
	e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
	defer e.Shutdown()
	e.backend = loginBackend{login: func(context.Context, LoginArgs, func(Event)) error {
		panic("backend failed")
	}}
	id, got := collect(t, e, events, `{"cmd":"login","args":{}}`)
	if len(got) != 1 || got[0].RequestID == nil || *got[0].RequestID != id {
		t.Fatalf("events: %#v", got)
	}
	response := got[0].Data.(Response)
	if response.OK || response.Error == nil || response.Error.Code != "internal" {
		t.Fatalf("response: %#v", response)
	}
}

func TestNoEventAfterShutdown(t *testing.T) {
	t.Run("external goroutine", func(t *testing.T) {
		events := make(chan Event, 10)
		release := make(chan struct{})
		done := make(chan struct{})
		e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
		defer e.Shutdown()
		e.backend = loginBackend{login: func(_ context.Context, _ LoginArgs, emit func(Event)) error {
			go func() {
				defer close(done)
				<-release
				emit(Event{Type: EventConnectionState, Data: ConnectionState{State: "connected"}})
			}()
			return nil
		}}
		collect(t, e, events, `{"cmd":"login","args":{}}`)
		e.Shutdown()
		close(release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("late emitter did not finish")
		}
		if len(events) != 0 {
			t.Fatalf("received %d events after shutdown", len(events))
		}
		if id := e.Call([]byte(`{"cmd":"logout","args":{}}`)); id != -1 {
			t.Fatalf("call after shutdown: %d", id)
		}
	})
	t.Run("backend ignores context", func(t *testing.T) {
		events := make(chan Event, 10)
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
		e.backend = loginBackend{login: func(_ context.Context, _ LoginArgs, emit func(Event)) error {
			close(started)
			<-release
			emit(Event{Type: EventConnectionState, Data: ConnectionState{State: "connected"}})
			return nil
		}}
		e.Call([]byte(`{"cmd":"login","args":{}}`))
		<-started
		shutdownDone := make(chan struct{})
		go func() { e.Shutdown(); close(shutdownDone) }()
		select {
		case <-shutdownDone:
		case <-time.After(7 * time.Second):
			t.Fatal("shutdown did not time out")
		}
		// Join the worker to check both its event and deferred response.
		releaseOnce.Do(func() { close(release) })
		e.wg.Wait()
		if len(events) != 0 {
			t.Fatalf("received %d events after shutdown timeout", len(events))
		}
	})
}

func TestRequestIDMonotonicAcrossEngines(t *testing.T) {
	var previous int64
	for i := 0; i < 3; i++ {
		events := make(chan Event, 10)
		e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
		id, _ := collect(t, e, events, `{"cmd":"logout","args":{}}`)
		e.Shutdown()
		if id <= previous {
			t.Fatalf("request ID %d did not increase from %d", id, previous)
		}
		previous = id
	}
}

func TestBadArgsVsBackendError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   string
		err    error
		code   string
		called bool
	}{
		{"decode", `{"cookies":true}`, nil, "bad_request", false},
		{"backend code", `{}`, fmt.Errorf("wrapped: %w", &Error{Code: "auth_required", Message: "expired"}), "auth_required", true},
		{"backend plain", `{}`, errors.New("backend failed"), "internal", true},
		{"backend empty code", `{}`, &Error{Message: "backend failed"}, "internal", true},
		{"backend JSON error", `{}`, &json.UnmarshalTypeError{Value: "number", Type: reflect.TypeOf("")}, "internal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan Event, 10)
			e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
			defer e.Shutdown()
			called := false
			e.backend = loginBackend{login: func(context.Context, LoginArgs, func(Event)) error {
				called = true
				return tc.err
			}}
			_, got := collect(t, e, events, `{"cmd":"login","args":`+tc.args+`}`)
			response := got[len(got)-1].Data.(Response)
			if response.OK || response.Error == nil || response.Error.Code != tc.code || called != tc.called {
				t.Fatalf("response: %#v, backend called: %v", response, called)
			}
		})
	}
}

func TestConcurrentCallsEachGetOneResponse(t *testing.T) {
	const count = 100
	events := make(chan Event, count*2)
	e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
	defer e.Shutdown()
	ids := make(chan int64, count)
	var callers sync.WaitGroup
	for i := 0; i < count; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			ids <- e.Call([]byte(`{"cmd":"logout","args":{}}`))
		}()
	}
	callers.Wait()
	e.Shutdown()
	close(ids)
	close(events)
	responses := make(map[int64]int)
	for event := range events {
		if event.Type != EventResponse || event.RequestID == nil || !event.Data.(Response).OK {
			t.Fatalf("unexpected event: %#v", event)
		}
		responses[*event.RequestID]++
	}
	if len(responses) != count {
		t.Fatalf("got responses for %d IDs, want %d", len(responses), count)
	}
	for id := range ids {
		if id <= 0 || responses[id] != 1 {
			t.Fatalf("request %d received %d responses", id, responses[id])
		}
	}
}

func TestFakeTimestampsAreRecent(t *testing.T) {
	var messages int
	before := time.Now().Add(-time.Hour).UnixMilli()
	_ = (FakeBackend{}).Login(context.Background(), LoginArgs{}, func(event Event) {
		if event.Type == EventMessageUpsert {
			messages++
			if msg := event.Data.(Message); msg.TS < before || msg.TS > time.Now().UnixMilli() {
				t.Errorf("fake timestamp is not recent: %d", msg.TS)
			}
		}
	})
	if messages == 0 {
		t.Fatal("no fake messages")
	}
}

func TestFakeFetchPagingAndRestore(t *testing.T) {
	events := make(chan Event, 200)
	e := New(Config{Backend: "fake"}, func(ev Event) { events <- ev })
	defer e.Shutdown()
	_, got := collect(t, e, events, `{"cmd":"fetch_messages","args":{"conversation_id":"u:1","limit":50}}`)
	data := responseData(t, got[len(got)-1])["data"].(map[string]any)
	if len(got) != 51 || data["has_more"] != true {
		t.Fatalf("first page: %d events, %v", len(got), data)
	}
	_, got = collect(t, e, events, fmt.Sprintf(`{"cmd":"fetch_messages","args":{"conversation_id":"u:1","limit":50,"cursor":%q}}`, data["cursor"]))
	if data = responseData(t, got[len(got)-1])["data"].(map[string]any); len(got) != 11 || data["has_more"] != false {
		t.Fatalf("last page: %d events, %v", len(got), data)
	}
	_, got = collect(t, e, events, `{"cmd":"fetch_messages","args":{"conversation_id":"u:2"}}`)
	if data = responseData(t, got[len(got)-1])["data"].(map[string]any); data["needs_restore"] != true || data["has_more"] != false {
		t.Fatalf("E2EE before restore: %v", data)
	}
	_, got = collect(t, e, events, `{"cmd":"restore_history","args":{"pin":"123456"}}`)
	if last := got[len(got)-2]; last.Type != EventHistoryRestoreState || responseData(t, last)["state"] != "done" {
		t.Fatalf("restore: %#v", got)
	}
	_, got = collect(t, e, events, `{"cmd":"fetch_conversations","args":{}}`)
	if data = responseData(t, got[len(got)-1])["data"].(map[string]any); len(got) != 11 || data["has_more"] != true {
		t.Fatalf("conversations: %d events, %v", len(got), data)
	}
}
