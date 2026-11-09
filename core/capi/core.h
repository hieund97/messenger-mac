#ifndef MESSENGER_CORE_H
#define MESSENGER_CORE_H
#include <stdint.h>

/* Go frees event_json after the callback returns. Swift must copy the string
 * during the callback and must not pass it to core_free.
 * Callbacks are serial (never concurrent), on arbitrary threads.
 * Callbacks must return promptly and must not call core_shutdown.
 * Calling core_call from a callback is allowed. */
typedef void (*core_event_cb)(const char *event_json);
/* Returns 0 on success, -1 for invalid config JSON, -2 for an unsupported
 * backend, -3 if already initialized, -4 while shutdown is in progress. */
int32_t core_init(const char *config_json, core_event_cb cb);
/* Returns request_id, or -1 if uninitialized/shutting down (no response).
 * A response may arrive BEFORE core_call returns. The caller must register
 * its pending request under the same lock as core_call, or buffer unmatched
 * responses until the pending request is registered. */
int64_t core_call(const char *request_json);
void    core_free(char *ptr);
void    core_shutdown(void);

#endif
