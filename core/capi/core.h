#ifndef MESSENGER_CORE_H
#define MESSENGER_CORE_H
#include <stdint.h>

typedef void (*core_event_cb)(const char *event_json);
int32_t core_init(const char *config_json, core_event_cb cb);   // 0 = OK
int64_t core_call(const char *request_json);                    // trả request_id
void    core_free(char *ptr);
void    core_shutdown(void);

#endif
