package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"macchat/core/engine"
)

func main() {
	cookiePath := flag.String("cookies", "", "path to cookie JSON")
	platform := flag.String("platform", "messenger", "messenger or facebook")
	data := flag.String("data", "./devdata", "data directory")
	duration := flag.Duration("duration", 2*time.Minute, "session duration")
	flag.Parse()
	raw, err := os.ReadFile(*cookiePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var values map[string]string
	if err = json.Unmarshal(raw, &values); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	list := make([]engine.Cookie, 0, len(values))
	for name, value := range values {
		list = append(list, engine.Cookie{Name: name, Value: value})
	}
	e := engine.New(engine.Config{DataDir: *data, Backend: "meta"}, func(event engine.Event) {
		if event.Type == engine.EventCookiesUpdated {
			if update, ok := event.Data.(engine.CookiesUpdated); ok {
				for i := range update.Cookies {
					update.Cookies[i].Value = "[redacted]"
				}
				event.Data = update
			}
		}
		if out, err := json.Marshal(event); err == nil {
			fmt.Println(string(out))
		}
	})
	defer e.Shutdown()
	req, _ := json.Marshal(map[string]any{"cmd": "login", "args": engine.LoginArgs{Cookies: list, Platform: *platform}})
	e.Call(req)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-time.After(*duration):
	case <-sig:
	}
}
