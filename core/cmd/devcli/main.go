package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"mime"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"macmess/core/engine"
)

func main() {
	cookiePath := flag.String("cookies", "", "path to cookie JSON")
	platform := flag.String("platform", "messenger", "messenger or facebook")
	data := flag.String("data", "./devdata", "data directory")
	duration := flag.Duration("duration", 2*time.Minute, "session duration")
	redact := flag.Bool("redact", true, "redact message text and attachment refs (-redact=false to show)")
	// The action flags take their second value from the arguments after the flags, e.g.
	//   devcli -cookies c.json -send-text u:123 hello there
	//   devcli -cookies c.json -send-file u:123 /path/to/file.pdf
	//   devcli -cookies c.json -download 'fb:...' /path/to/new-file
	sendText := flag.String("send-text", "", "conversation_id; sends the remaining arguments as text")
	sendFile := flag.String("send-file", "", "conversation_id; sends the file named by the first remaining argument")
	download := flag.String("download", "", "attachment ref; writes it to the path named by the first remaining argument")
	settle := flag.Duration("settle", 15*time.Second, "wait after connecting before running an action (E2EE and thread list)")
	secretsFile := flag.String("secrets-file", "", "file holding the secure_history_device secret (passed in login.secrets)")
	// Probe mode: prints only counts, timestamps and flags to stderr, never names, text or secrets.
	fetchConvs := flag.Int("fetch-conversations", 0, "probe: page fetch_conversations up to N times")
	fetchMsgs := flag.String("fetch-messages", "", "probe: comma-separated conversation_ids to page with fetch_messages")
	pages := flag.Int("pages", 5, "probe: fetch_messages pages per conversation")
	limit := flag.Int("limit", 30, "probe: fetch_messages limit")
	listConvs := flag.Bool("list-conversations", false, "probe: print the conversation summary (ids, counts) at the end")
	flag.Parse()
	probing := *fetchConvs > 0 || *fetchMsgs != "" || *listConvs
	action, err := actionRequest(*sendText, *sendFile, *download, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	raw, err := os.ReadFile(*cookiePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Either {"name": "value"} or the [{"name","value","domain"}] list the app keeps in the Keychain.
	var list []engine.Cookie
	if err = json.Unmarshal(raw, &list); err != nil {
		var values map[string]string
		if err = json.Unmarshal(raw, &values); err != nil {
			fmt.Fprintln(os.Stderr, "invalid cookie file")
			os.Exit(1)
		}
		for name, value := range values {
			list = append(list, engine.Cookie{Name: name, Value: value})
		}
	}
	var secrets map[string]string
	if *secretsFile != "" {
		secret, err := os.ReadFile(*secretsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot read secrets file")
			os.Exit(1)
		}
		secrets = map[string]string{engine.SecretSecureHistoryDevice: strings.TrimSpace(string(secret))}
	}
	connected := make(chan struct{}, 1)
	probe := newProber()
	e := engine.New(engine.Config{DataDir: *data, Backend: "meta"}, func(event engine.Event) {
		if probing {
			if s, ok := event.Data.(engine.ConnectionState); ok && s.State == "connected" {
				select {
				case connected <- struct{}{}:
				default:
				}
			}
			if probe.observe(event) {
				return
			}
			// Everything else is printed below with the usual redaction; secret values never are.
			if event.Type == engine.EventSecretStore {
				fmt.Fprintln(os.Stderr, "event secret_store (value not shown)")
				return
			}
		}
		if s, ok := event.Data.(engine.ConnectionState); ok && s.State == "connected" {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
		if *redact && event.Type == engine.EventMessageUpsert {
			if message, ok := event.Data.(engine.Message); ok {
				if message.Text != nil {
					text := "[redacted]"
					message.Text = &text
				}
				attachments := make([]engine.Attachment, len(message.Attachments))
				for i, a := range message.Attachments {
					a.Ref = "[redacted]"
					a.PreviewURL = nil
					attachments[i] = a
				}
				message.Attachments = attachments
				event.Data = message
			}
		}
		if *redact && event.Type == engine.EventSecretStore {
			if secret, ok := event.Data.(engine.SecretStore); ok {
				secret.Value = "[redacted]"
				event.Data = secret
			}
		}
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
	probe.e = e
	req, _ := json.Marshal(map[string]any{"cmd": "login", "args": engine.LoginArgs{Cookies: list, Platform: *platform, Secrets: secrets}})
	e.Call(req)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	end := time.After(*duration)
	if probing {
		select {
		case <-connected:
		case <-end:
			fmt.Fprintln(os.Stderr, "devcli: never connected")
			return
		case <-sig:
			return
		}
		select {
		case <-time.After(*settle):
		case <-sig:
			return
		}
		total, restored := probe.snapshot()
		fmt.Fprintf(os.Stderr, "after login+settle: conversations=%d restored=%d\n", total, restored)
		if *fetchConvs > 0 {
			probe.fetchConversations(*fetchConvs)
		}
		if *fetchMsgs != "" {
			for _, conv := range strings.Split(*fetchMsgs, ",") {
				probe.fetchMessages(strings.TrimSpace(conv), *pages, *limit)
			}
		}
		if *listConvs {
			probe.listConversations()
		}
		return
	}
	if action != nil {
		select {
		case <-connected:
			select {
			case <-time.After(*settle):
				fmt.Fprintln(os.Stderr, "devcli: running action")
				e.Call(action)
			case <-end:
				return
			case <-sig:
				return
			}
		case <-end:
			fmt.Fprintln(os.Stderr, "devcli: never connected, action skipped")
			return
		case <-sig:
			return
		}
	}
	select {
	case <-end:
	case <-sig:
	}
}

// actionRequest builds the one command to run after connecting, or nil when no action flag is set.
func actionRequest(sendText, sendFile, download string, rest []string) ([]byte, error) {
	set := 0
	for _, v := range []string{sendText, sendFile, download} {
		if v != "" {
			set++
		}
	}
	if set == 0 {
		return nil, nil
	} else if set > 1 {
		return nil, fmt.Errorf("use only one of -send-text, -send-file, -download")
	}
	var cmd string
	var args any
	switch {
	case sendText != "":
		text := strings.Join(rest, " ")
		if text == "" {
			return nil, fmt.Errorf("-send-text needs the text after the flags")
		}
		cmd, args = engine.CmdSendText, engine.SendTextArgs{ConversationID: sendText, Text: text, ClientID: uuid.NewString()}
	case sendFile != "":
		if len(rest) != 1 {
			return nil, fmt.Errorf("-send-file needs one file path after the flags")
		}
		path, err := filepath.Abs(rest[0])
		if err != nil {
			return nil, err
		}
		cmd, args = engine.CmdSendMedia, engine.SendMediaArgs{ConversationID: sendFile, FilePath: path, MIME: mime.TypeByExtension(filepath.Ext(path)), ClientID: uuid.NewString()}
	default:
		if len(rest) != 1 {
			return nil, fmt.Errorf("-download needs one destination path after the flags")
		}
		dest, err := filepath.Abs(rest[0])
		if err != nil {
			return nil, err
		}
		cmd, args = engine.CmdDownloadMedia, engine.DownloadMediaArgs{AttachmentRef: download, DestPath: dest}
	}
	return json.Marshal(map[string]any{"cmd": cmd, "args": args})
}
