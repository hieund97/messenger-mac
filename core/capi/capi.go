package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef void (*core_event_cb)(const char *event_json);
*/
import "C"

import "unsafe"

//export core_init
func core_init(configJSON *C.char, cb C.core_event_cb) C.int32_t { return 0 }

//export core_call
func core_call(requestJSON *C.char) C.int64_t { return 1 }

//export core_free
func core_free(ptr *C.char) { C.free(unsafe.Pointer(ptr)) }

//export core_shutdown
func core_shutdown() {}

func main() {}
