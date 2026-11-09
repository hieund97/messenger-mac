package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef void (*core_event_cb)(const char *event_json);
static inline void call_event_cb(core_event_cb cb, const char *event_json) { cb(event_json); }
*/
import "C"

import (
	"encoding/json"
	"sync"
	"unsafe"

	"macmess/core/engine"
)

var state struct {
	sync.Mutex
	engine       *engine.Engine
	shuttingDown bool
}

//export core_init
func core_init(configJSON *C.char, cb C.core_event_cb) C.int32_t {
	state.Lock()
	defer state.Unlock()
	if state.shuttingDown {
		return -4
	}
	if state.engine != nil {
		return -3
	}
	var cfg engine.Config
	if configJSON == nil || json.Unmarshal([]byte(C.GoString(configJSON)), &cfg) != nil {
		return -1
	}
	if cfg.Backend != "fake" && cfg.Backend != "" && cfg.Backend != "meta" {
		return -2
	}
	state.engine = engine.New(cfg, func(event engine.Event) {
		defer func() { _ = recover() }()
		if cb == nil {
			return
		}
		data, err := json.Marshal(event)
		if err != nil {
			if event.Type != engine.EventResponse {
				return
			}
			data, err = json.Marshal(engine.Event{
				Type: engine.EventResponse, RequestID: event.RequestID,
				Data: engine.Response{Error: &engine.Error{Code: "internal", Message: "marshal failed"}},
			})
			if err != nil {
				return
			}
		}
		s := C.CString(string(data))
		defer C.free(unsafe.Pointer(s))
		C.call_event_cb(cb, s)
	})
	return 0
}

//export core_call
func core_call(requestJSON *C.char) C.int64_t {
	state.Lock()
	defer state.Unlock()
	if state.engine == nil || state.shuttingDown {
		return -1
	}
	var raw []byte
	if requestJSON != nil {
		raw = []byte(C.GoString(requestJSON))
	}
	return C.int64_t(state.engine.Call(raw))
}

//export core_free
func core_free(ptr *C.char) { C.free(unsafe.Pointer(ptr)) }

//export core_shutdown
func core_shutdown() {
	state.Lock()
	if state.engine == nil || state.shuttingDown {
		state.Unlock()
		return
	}
	e := state.engine
	state.shuttingDown = true
	state.Unlock()
	e.Shutdown()
	state.Lock()
	state.engine = nil
	state.shuttingDown = false
	state.Unlock()
}
func main() {}
