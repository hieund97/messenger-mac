package main
/*
#include <stdlib.h>
typedef void (*spike_callback)(const char*);
static inline void call_callback(spike_callback cb, const char *s) { cb(s); }
*/
import "C"
import ("context"; "encoding/json"; "fmt"; "sync"; "unsafe"; "spike/core")
var mu sync.Mutex
var cancel context.CancelFunc
var done chan struct{}
//export spike_start
func spike_start(raw *C.char, dir *C.char, cb C.spike_callback) C.int {
 if raw==nil || dir==nil || cb==nil {return 1}
 emit:=func(e core.Event){b,_:=json.Marshal(e); s:=C.CString(string(b)); defer C.free(unsafe.Pointer(s)); C.call_callback(cb,s)}
 c,err:=core.ParseCookies([]byte(C.GoString(raw)),"messenger"); if err!=nil {emit(core.Event{"type":"error","error":err.Error()}); return 1}
 mu.Lock(); if cancel!=nil {mu.Unlock(); emit(core.Event{"type":"error","error":"already running"}); return 1}; ctx,stop:=context.WithCancel(context.Background()); cancel=stop; finished:=make(chan struct{}); done=finished; mu.Unlock()
 data:=C.GoString(dir); go func(){defer func(){mu.Lock(); cancel=nil; close(finished); mu.Unlock()}(); if err:=core.Run(ctx,c,data,emit); err!=nil {emit(core.Event{"type":"error","error":fmt.Sprint(err)})}}(); return 0
}
//export spike_stop
func spike_stop() {mu.Lock(); stop,ch:=cancel,done; mu.Unlock(); if stop!=nil {stop(); <-ch}}
func main() {}
