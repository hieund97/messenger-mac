package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
)

// cdnTransport serves fixed responses per URL and records what was requested.
type cdnTransport struct {
	mu       sync.Mutex
	status   map[string]int
	requests []*http.Request
}

func (c *cdnTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	status, ok := c.status[req.URL.String()]
	c.mu.Unlock()
	if !ok {
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("payload")), ContentLength: 7, Request: req, Header: http.Header{}}, nil
}

func newMediaBackend(t *testing.T, cdn *cdnTransport) (*MetaBackend, *fakeLS) {
	t.Helper()
	b, _, ls, _ := newSendBackend(t)
	b.mediaHTTP = &http.Client{Transport: cdn, CheckRedirect: checkMediaRedirect}
	return b, ls
}

func TestExpiredMediaURLRefreshedOn403(t *testing.T) {
	const oldURL = "https://scontent.xx.fbcdn.net/v/old.jpg?oe=1"
	const newURL = "https://scontent.xx.fbcdn.net/v/new.jpg?oe=2"
	cdn := &cdnTransport{status: map[string]int{oldURL: http.StatusForbidden, newURL: http.StatusOK}}
	b, ls := newMediaBackend(t, cdn)
	ls.execute = func(_ context.Context, tasks []socket.Task) (*table.LSTable, error) {
		fetch, ok := tasks[0].(*socket.FetchMessagesTask)
		if !ok || fetch.ThreadKey != testPeer || fetch.ReferenceMessageId != "mid.$img" || fetch.ReferenceTimestampMs != 5001 {
			t.Errorf("refresh task = %+v", tasks[0])
		}
		return &table.LSTable{LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{
			{AttachmentFbid: "777", PlayableUrl: newURL, MessageId: "mid.$img", ThreadKey: testPeer},
		}}, nil
	}
	ref := encodeMediaRef(mediaRef{Kind: mediaRefFB, URL: oldURL, FBID: "777", MessageID: "mid.$img", ThreadKey: testPeer, TimestampMS: 5000, MIME: "image/jpeg"})
	dest := filepath.Join(t.TempDir(), "payload")
	if err := b.DownloadMedia(context.Background(), DownloadMediaArgs{AttachmentRef: ref, DestPath: dest}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "payload" {
		t.Fatalf("data = %q, %v", data, err)
	}
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	if n := len(ls.calls()); n != 1 {
		t.Fatalf("refresh calls = %d", n)
	}
	cdn.mu.Lock()
	defer cdn.mu.Unlock()
	if len(cdn.requests) != 2 || cdn.requests[0].URL.String() != oldURL || cdn.requests[1].URL.String() != newURL {
		t.Fatalf("requests = %d", len(cdn.requests))
	}
	for _, r := range cdn.requests {
		if r.Header.Get("Cookie") != "" {
			t.Fatal("cookies must never be sent to the CDN")
		}
	}
}

func TestDownloadRejectsDisallowedHostAndSymlink(t *testing.T) {
	cdn := &cdnTransport{status: map[string]int{"https://scontent.xx.fbcdn.net/ok.jpg": http.StatusOK}}
	b, _ := newMediaBackend(t, cdn)
	download := func(ref, dest string) error {
		return b.DownloadMedia(context.Background(), DownloadMediaArgs{AttachmentRef: ref, DestPath: dest}, nil)
	}
	code := func(err error) string {
		var e *Error
		if errors.As(err, &e) {
			return e.Code
		}
		return ""
	}
	dir := t.TempDir()
	for _, u := range []string{
		"https://evil.example.com/a.jpg",
		"https://fbcdn.net.evil.com/a.jpg",
		"https://evilfbcdn.net/a.jpg",
		"http://scontent.xx.fbcdn.net/a.jpg",
		"https://scontent.xx.fbcdn.net:8443/a.jpg",
		"https://user@scontent.xx.fbcdn.net/a.jpg",
	} {
		ref := encodeMediaRef(mediaRef{Kind: mediaRefFB, URL: u})
		if err := download(ref, filepath.Join(dir, "x")); code(err) != "media_host_not_allowed" {
			t.Fatalf("%s: err = %v", u, err)
		}
	}
	cdn.mu.Lock()
	if len(cdn.requests) != 0 {
		t.Fatal("disallowed hosts must not be contacted")
	}
	cdn.mu.Unlock()

	good := encodeMediaRef(mediaRef{Kind: mediaRefFB, URL: "https://scontent.xx.fbcdn.net/ok.jpg"})
	// Parent directory is a symlink.
	real := t.TempDir()
	link := filepath.Join(dir, "linkdir")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := download(good, filepath.Join(link, "payload")); code(err) != "bad_request" {
		t.Fatalf("symlinked parent: err = %v", err)
	}
	// Destination is a symlink to an existing file.
	target := filepath.Join(real, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "payload")
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	if err := download(good, dest); err == nil {
		t.Fatal("symlinked destination accepted")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatal("symlink target overwritten")
	}
	// Relative destination.
	if err := download(good, "payload"); code(err) != "bad_request" {
		t.Fatalf("relative dest: err = %v", err)
	}
	// E2EE refs: the direct path is joined onto whatsmeow's own media host.
	for _, dp := range []string{"https://evil.com/x", "//evil.com/x", "v/t62/x", "/v/t62\\x", "/v/t62/x\n"} {
		ref := encodeMediaRef(mediaRef{Kind: mediaRefWA, DirectPath: dp, MediaType: "image", MediaKey: make([]byte, 32), SHA256: make([]byte, 32), EncSHA256: make([]byte, 32)})
		if err := download(ref, filepath.Join(dir, "wa")); code(err) != "bad_request" {
			t.Fatalf("direct path %q: err = %v", dp, err)
		}
	}
	if err := download("fb:!!!", filepath.Join(dir, "bad")); code(err) != "bad_request" {
		t.Fatalf("bad ref: err = %v", err)
	}
}

func TestRefreshesOfOneThreadShareOneFetch(t *testing.T) {
	const old1, old2 = "https://scontent.xx.fbcdn.net/v/a.jpg?oe=1", "https://scontent.xx.fbcdn.net/v/b.jpg?oe=1"
	const new1, new2 = "https://scontent.xx.fbcdn.net/v/a.jpg?oe=2", "https://scontent.xx.fbcdn.net/v/b.jpg?oe=2"
	cdn := &cdnTransport{status: map[string]int{old1: http.StatusForbidden, old2: http.StatusForbidden, new1: http.StatusOK, new2: http.StatusOK}}
	b, ls := newMediaBackend(t, cdn)
	release := make(chan struct{})
	ls.execute = func(context.Context, []socket.Task) (*table.LSTable, error) {
		<-release
		return &table.LSTable{LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{
			{AttachmentFbid: "1", PlayableUrl: new1, ThreadKey: testPeer},
			{AttachmentFbid: "2", PlayableUrl: new2, ThreadKey: testPeer},
		}}, nil
	}
	dir := t.TempDir()
	errs := make(chan error, 2)
	for i, u := range []string{old1, old2} {
		fbid := strconv.Itoa(i + 1)
		ref := encodeMediaRef(mediaRef{Kind: mediaRefFB, URL: u, FBID: fbid, MessageID: "mid.$" + fbid, ThreadKey: testPeer, TimestampMS: 5000, MIME: "image/jpeg"})
		go func() {
			errs <- b.DownloadMedia(context.Background(), DownloadMediaArgs{AttachmentRef: ref, DestPath: filepath.Join(dir, fbid)}, nil)
		}()
	}
	time.Sleep(200 * time.Millisecond) // both downloads reach the refresh
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := len(ls.calls()); n != 1 {
		t.Fatalf("FetchMessagesTask sent %d times", n)
	}
}

func TestMediaRefreshDoesNotEmitHistory(t *testing.T) {
	const oldURL, newURL = "https://scontent.xx.fbcdn.net/v/a.jpg?oe=1", "https://scontent.xx.fbcdn.net/v/a.jpg?oe=2"
	b, log, ls, _ := newSendBackend(t)
	b.mediaHTTP = &http.Client{Transport: &cdnTransport{status: map[string]int{oldURL: http.StatusForbidden, newURL: http.StatusOK}}, CheckRedirect: checkMediaRedirect}
	page := func(id string) *table.LSTable {
		return &table.LSTable{
			LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{ThreadKey: testPeer}},
			LSUpsertMessage:         []*table.LSUpsertMessage{{ThreadKey: testPeer, MessageId: id, SenderId: testPeer, TimestampMs: 100, Text: "old"}},
		}
	}
	// Without a refresh, an upserted page is emitted as usual.
	b.handleTable(b.client, page("mid.$before"))
	if n := len(log.ofType(EventMessageUpsert)); n != 1 {
		t.Fatalf("control: upserts = %d", n)
	}
	ls.execute = func(context.Context, []socket.Task) (*table.LSTable, error) {
		return &table.LSTable{LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{{AttachmentFbid: "1", PlayableUrl: newURL, ThreadKey: testPeer}}}, nil
	}
	ref := encodeMediaRef(mediaRef{Kind: mediaRefFB, URL: oldURL, FBID: "1", MessageID: "mid.$img", ThreadKey: testPeer, TimestampMS: 5000, MIME: "image/jpeg"})
	if err := b.DownloadMedia(context.Background(), DownloadMediaArgs{AttachmentRef: ref, DestPath: filepath.Join(t.TempDir(), "a")}, nil); err != nil {
		t.Fatal(err)
	}
	// The re-fetched page arrives over the socket after the response.
	b.handleTable(b.client, page("mid.$history"))
	if n := len(log.ofType(EventMessageUpsert)); n != 1 {
		t.Fatalf("history of the refresh emitted: upserts = %d", n)
	}
	// New messages in the same thread still go through.
	b.handleTable(b.client, &table.LSTable{LSInsertMessage: []*table.LSInsertMessage{{ThreadKey: testPeer, MessageId: "mid.$new", SenderId: testPeer, TimestampMs: 9000, Text: "new"}}})
	if n := len(log.ofType(EventMessageUpsert)); n != 2 {
		t.Fatalf("new message: upserts = %d", n)
	}
}

func TestMediaClientReusedWithoutJar(t *testing.T) {
	b := NewMetaBackend(Config{})
	c := b.mediaClient()
	if c != b.mediaClient() || c.Jar != nil || c.CheckRedirect == nil || c.Transport == nil {
		t.Fatalf("client = %+v", c)
	}
}
