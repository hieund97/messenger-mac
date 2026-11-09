package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"google.golang.org/protobuf/proto"
)

const (
	maxDownloadBytes    = 100 * 1024 * 1024
	downloadTimeout     = 5 * time.Minute
	mediaRefreshTimeout = 15 * time.Second
	// mediaExpiryMargin treats a CDN URL as expired a little early, like directmedia.go:66.
	mediaExpiryMargin = 5 * time.Minute
	maxMediaRedirects = 5
	// mediaRefreshGrace keeps a refreshed thread's history muted a little after the refresh, for
	// re-fetched rows that arrive over the socket late.
	mediaRefreshGrace = 10 * time.Second
	// refreshedURLTTL is how long a URL from a refresh is reused for other attachments of the page.
	refreshedURLTTL       = 10 * time.Minute
	refreshedURLsCapacity = 1024
)

// allowedMediaDomains are the only hosts download_media fetches from (and their subdomains). E2EE
// media never uses this list: whatsmeow builds those URLs from its own media connection hosts.
var allowedMediaDomains = []string{"fbcdn.net", "fbsbx.com", "whatsapp.net"}

var (
	errMediaHostNotAllowed = errors.New("media host not allowed")
	errMediaForbidden      = errors.New("media URL forbidden")
	errMediaHTTP           = errors.New("media request failed")
	errMediaTooLarge       = errors.New("media too large")
)

// readUpload reads a file to send. Errors use the codes Swift shows on the message bubble.
func readUpload(path string) ([]byte, string, error) {
	unreadable := &Error{Code: "file_unreadable", Message: "cannot read file"}
	if path == "" {
		return nil, "", unreadable
	}
	// Check the type before opening: opening a FIFO blocks, a device may have side effects.
	pre, err := os.Stat(path)
	if err != nil || !pre.Mode().IsRegular() {
		return nil, "", unreadable
	}
	// O_NONBLOCK keeps a FIFO swapped in after the Stat from blocking the open; the file must still be
	// the same regular file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", unreadable
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || !os.SameFile(pre, st) {
		return nil, "", unreadable
	}
	tooLarge := &Error{Code: "file_too_large", Message: "file exceeds 25 MB"}
	if st.Size() > maxUploadBytes {
		return nil, "", tooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		return nil, "", unreadable
	}
	if len(data) > maxUploadBytes {
		return nil, "", tooLarge
	}
	return data, filepath.Base(path), nil
}

func uploadMIME(declared string, data []byte) string {
	if t, _, err := mime.ParseMediaType(declared); err == nil && strings.Contains(t, "/") {
		return t
	}
	t, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	return t
}

// safeFilename keeps the multipart header of the Mercury upload well-formed (mercury.go writes the
// name into a quoted Content-Disposition parameter as is).
func safeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." {
		return "file"
	}
	return name
}

func (r realLS) UploadMedia(ctx context.Context, threadID int64, filename, mimeType string, data []byte) (int64, error) {
	resp, err := r.c.GetHTTP().SendMercuryUploadRequest(ctx, threadID, &httpclient.MercuryUploadMedia{
		Filename:  safeFilename(filename),
		MimeType:  mimeType,
		MediaData: data,
	})
	if err != nil {
		return 0, err
	}
	if resp.Payload.RealMetadata == nil {
		return 0, errors.New("upload response without metadata")
	}
	fbid := resp.Payload.RealMetadata.GetFbId()
	if fbid == 0 {
		return 0, errors.New("upload response without fbid")
	}
	return fbid, nil
}

// Images sent as ImageMessage; other types (GIF and video included) go as documents.
var waImageMIMEs = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

// waMediaMessage uploads data and wraps it like msgconv.reuploadMediaToWhatsApp/wrapWhatsAppMedia
// (to-whatsapp.go:247-460): images as ImageMessage with dimensions, everything else as DocumentMessage.
func waMediaMessage(ctx context.Context, wa waOps, data []byte, mimeType, filename string) (*waConsumerApplication.ConsumerApplication, error) {
	isImage := waImageMIMEs[mimeType]
	var w, h int
	mediaType := whatsmeow.MediaDocument
	if isImage {
		mediaType = whatsmeow.MediaImage
		if mimeType == "image/png" {
			data = trimPNGTrailer(data)
		}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			w, h = cfg.Width, cfg.Height
		}
	}
	uploaded, err := wa.Upload(ctx, data, mediaType)
	if err != nil {
		return nil, err
	}
	tw, th := clampTo400(w, h)
	if tw == 0 && isImage {
		tw, th = 400, 400
	}
	transport := &waMediaTransport.WAMediaTransport{
		Integral: &waMediaTransport.WAMediaTransport_Integral{
			FileSHA256:        uploaded.FileSHA256,
			MediaKey:          uploaded.MediaKey,
			FileEncSHA256:     uploaded.FileEncSHA256,
			DirectPath:        proto.String(uploaded.DirectPath),
			MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		},
		Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{
			FileLength: proto.Uint64(uint64(len(data))),
			Mimetype:   proto.String(mimeType),
			// Messenger iOS and Android do not show media without the thumbnail dimensions.
			Thumbnail: &waMediaTransport.WAMediaTransport_Ancillary_Thumbnail{
				ThumbnailWidth:  proto.Uint32(uint32(tw)),
				ThumbnailHeight: proto.Uint32(uint32(th)),
			},
			ObjectID: proto.String(uploaded.ObjectID),
		},
	}
	content := &waConsumerApplication.ConsumerApplication_Content{}
	if isImage {
		img := &waConsumerApplication.ConsumerApplication_ImageMessage{Caption: &waCommon.MessageText{}}
		err = img.Set(&waMediaTransport.ImageTransport{
			Integral:  &waMediaTransport.ImageTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.ImageTransport_Ancillary{Width: proto.Uint32(uint32(w)), Height: proto.Uint32(uint32(h))},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: img}
	} else {
		doc := &waConsumerApplication.ConsumerApplication_DocumentMessage{FileName: proto.String(filename)}
		err = doc.Set(&waMediaTransport.DocumentTransport{
			Integral:  &waMediaTransport.DocumentTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.DocumentTransport_Ancillary{},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_DocumentMessage{DocumentMessage: doc}
	}
	if err != nil {
		return nil, err
	}
	return consumerMessage(content), nil
}

func clampTo400(w, h int) (int, int) {
	if w > 400 {
		h = h * 400 / w
		w = 400
	}
	if h > 400 {
		w = w * 400 / h
		h = 400
	}
	return w, h
}

// trimPNGTrailer drops bytes after the IEND chunk, which Messenger Android rejects (to-whatsapp.go:221).
func trimPNGTrailer(data []byte) []byte {
	const pngHeader = "\x89PNG\r\n\x1a\n"
	if !bytes.HasPrefix(data, []byte(pngHeader)) {
		return data
	}
	for pos := 8; pos+12 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		end := pos + 12 + length
		if end > len(data) || end <= pos || length < 0 {
			break
		}
		if string(data[pos+4:pos+8]) == "IEND" {
			if length != 0 {
				break
			}
			return data[:end]
		}
		pos = end
	}
	return data
}

// mediaHostAllowed accepts only https URLs on the default port of an allowed domain, without
// credentials in the URL.
func mediaHostAllowed(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range allowedMediaDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// checkMediaRedirect applies the host allowlist to every redirect hop.
func checkMediaRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxMediaRedirects {
		return errMediaHTTP
	}
	if !mediaHostAllowed(req.URL) {
		return errMediaHostNotAllowed
	}
	return nil
}

// newMediaHTTPClient has no cookie jar: CDN requests never carry the session cookies. It has its own
// transport, so its connections are reused across downloads and not shared with anything else.
func newMediaHTTPClient() *http.Client {
	return &http.Client{
		Transport:     http.DefaultTransport.(*http.Transport).Clone(),
		Timeout:       downloadTimeout,
		CheckRedirect: checkMediaRedirect,
	}
}

// sharedMediaClient is the client of every CDN download.
var sharedMediaClient = newMediaHTTPClient()

var waMediaTypes = map[string]whatsmeow.MediaType{
	"image":    whatsmeow.MediaImage,
	"sticker":  whatsmeow.MediaImage,
	"video":    whatsmeow.MediaVideo,
	"audio":    whatsmeow.MediaAudio,
	"document": whatsmeow.MediaDocument,
}

// validDirectPath requires a plain absolute path, since whatsmeow appends it to its media host.
func validDirectPath(p string) bool {
	if len(p) > 2048 || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsRune(p, '\\') {
		return false
	}
	for _, r := range p {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse("https://media.invalid" + p)
	return err == nil && u.Host == "media.invalid"
}

// validateMediaRef checks a ref that came back from Swift before anything is fetched.
func validateMediaRef(ref mediaRef) error {
	switch ref.Kind {
	case mediaRefFB:
		u, err := url.Parse(ref.URL)
		if ref.URL == "" || err != nil {
			return &Error{Code: "bad_request", Message: "invalid media ref"}
		}
		if !mediaHostAllowed(u) {
			return &Error{Code: "media_host_not_allowed", Message: "media host not allowed"}
		}
	case mediaRefWA:
		if _, ok := waMediaTypes[ref.MediaType]; !ok || !validDirectPath(ref.DirectPath) ||
			len(ref.MediaKey) != 32 || len(ref.SHA256) != 32 || len(ref.EncSHA256) != 32 {
			return &Error{Code: "bad_request", Message: "invalid media ref"}
		}
	default:
		return &Error{Code: "bad_request", Message: "invalid media ref"}
	}
	return nil
}

// checkDest accepts an absolute, clean path whose parent is a real directory (not a symlink) and
// that does not exist yet.
func checkDest(dest string) error {
	bad := &Error{Code: "bad_request", Message: "invalid destination"}
	if dest == "" || !filepath.IsAbs(dest) || filepath.Clean(dest) != dest {
		return bad
	}
	st, err := os.Lstat(filepath.Dir(dest))
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return bad
	}
	if _, err = os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		return &Error{Code: "bad_request", Message: "destination exists"}
	}
	return nil
}

// createDest creates dest exclusively with mode 0600 and never follows a symlink. E2EE downloads
// need read access too: whatsmeow verifies and decrypts the file in place.
func createDest(dest string, readable bool) (*os.File, error) {
	flags := os.O_WRONLY
	if readable {
		flags = os.O_RDWR
	}
	return os.OpenFile(dest, flags|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
}

// DownloadMedia writes an attachment to dest_path. Errors never include the URL or ref.
func (b *MetaBackend) DownloadMedia(ctx context.Context, args DownloadMediaArgs, _ func(Event)) error {
	ref, err := decodeMediaRef(args.AttachmentRef)
	if err != nil {
		return &Error{Code: "bad_request", Message: "invalid media ref"}
	}
	if err = validateMediaRef(ref); err != nil {
		return err
	}
	if err = checkDest(args.DestPath); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	if ref.Kind == mediaRefWA {
		return b.downloadWA(ctx, ref, args.DestPath)
	}
	return b.downloadFB(ctx, ref, args.DestPath)
}

func (b *MetaBackend) mediaClient() *http.Client {
	if b.mediaHTTP != nil {
		return b.mediaHTTP
	}
	return sharedMediaClient
}

// fetchMedia GETs an allowlisted CDN URL with browser-like headers (mediadl/download.go:95).
func (b *MetaBackend) fetchMedia(ctx context.Context, rawURL, mimeType string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !mediaHostAllowed(u) {
		return nil, errMediaHostNotAllowed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errMediaHTTP
	}
	req.Header.Set("Accept", "*/*")
	dest := "empty"
	switch strings.Split(mimeType, "/")[0] {
	case "image":
		req.Header.Set("Accept", "image/avif,image/webp,*/*")
		dest = "image"
	case "video", "audio":
		dest = strings.Split(mimeType, "/")[0]
	}
	req.Header.Set("Sec-Fetch-Dest", dest)
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("User-Agent", useragent.UserAgent)
	req.Header.Set("sec-ch-ua", useragent.SecCHUserAgent)
	req.Header.Set("sec-ch-ua-platform", useragent.SecCHPlatform)
	resp, err := b.mediaClient().Do(req)
	if err != nil {
		if errors.Is(err, errMediaHostNotAllowed) {
			return nil, errMediaHostNotAllowed
		}
		return nil, errMediaHTTP
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			return nil, errMediaForbidden
		}
		return nil, errMediaHTTP
	}
	return resp, nil
}

func (b *MetaBackend) downloadFB(ctx context.Context, ref mediaRef, dest string) error {
	resp, err := b.fetchMedia(ctx, ref.URL, ref.MIME)
	expired := ref.ExpiresMS > 0 && time.Now().After(time.UnixMilli(ref.ExpiresMS).Add(-mediaExpiryMargin))
	// directmedia.go:69: a 403 (or a failure on a URL known to be expired) gets one refresh.
	if (errors.Is(err, errMediaForbidden) || (err != nil && expired)) && ref.FBID != "" && ref.MessageID != "" && ref.ThreadKey != 0 {
		if fresh, rerr := b.refreshMediaURL(ctx, ref); rerr == nil {
			resp, err = b.fetchMedia(ctx, fresh, ref.MIME)
		}
	}
	if err != nil {
		if errors.Is(err, errMediaHostNotAllowed) {
			return &Error{Code: "media_host_not_allowed", Message: "media host not allowed"}
		}
		return &Error{Code: "download_failed", Message: "media download failed"}
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxDownloadBytes {
		return &Error{Code: "media_too_large", Message: "media exceeds 100 MB"}
	}
	f, err := createDest(dest, false)
	if err != nil {
		return &Error{Code: "bad_request", Message: "cannot create destination"}
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxDownloadBytes {
		err = errMediaTooLarge
	}
	if err != nil {
		_ = os.Remove(dest)
		if errors.Is(err, errMediaTooLarge) {
			return &Error{Code: "media_too_large", Message: "media exceeds 100 MB"}
		}
		return &Error{Code: "download_failed", Message: "media download failed"}
	}
	return nil
}

func (b *MetaBackend) downloadWA(ctx context.Context, ref mediaRef, dest string) error {
	s, err := b.liveSession()
	if err != nil {
		return err
	}
	if s.wa == nil {
		return &Error{Code: "e2ee_unavailable", Message: "encrypted chats are not available"}
	}
	if err = s.wa.WaitReady(ctx); err != nil {
		return &Error{Code: "disconnected", Message: "encrypted chats are not connected"}
	}
	f, err := createDest(dest, true)
	if err != nil {
		return &Error{Code: "bad_request", Message: "cannot create destination"}
	}
	// whatsmeow decrypts and checks the SHA-256 of the plaintext.
	err = s.wa.DownloadToFile(ctx, ref, waMediaTypes[ref.MediaType], &limitedFile{f: f, max: maxDownloadBytes})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dest)
		if errors.Is(err, errMediaTooLarge) {
			return &Error{Code: "media_too_large", Message: "media exceeds 100 MB"}
		}
		return &Error{Code: "download_failed", Message: "media download failed"}
	}
	return nil
}

// refreshMediaURL re-fetches the message around the attachment and returns its new URL
// (directmedia.go:164 refreshBlobMedia). The URL comes in the response or over the socket. Refreshes of
// one thread run one at a time and share their results, so several expired images of a page cost one
// FetchMessagesTask (mautrix: mediaRefreshLock + getAlreadyRefreshedURL).
func (b *MetaBackend) refreshMediaURL(ctx context.Context, ref mediaRef) (string, error) {
	s, err := b.liveSession()
	if err != nil {
		return "", err
	}
	if s.ls == nil {
		return "", errMediaHTTP
	}
	ctx, cancel := context.WithTimeout(ctx, mediaRefreshTimeout)
	defer cancel()
	ch := b.waits.addMedia(ref.FBID)
	defer b.waits.removeMedia(ref.FBID, ch)
	unlock, err := b.refreshes.lock(ctx, ref.ThreadKey)
	if err != nil {
		return "", err
	}
	defer unlock()
	if fresh := b.refreshes.lookup(ref.FBID, ref.URL); fresh != "" {
		return fresh, nil
	}
	select {
	case fresh := <-ch: // came over the socket while waiting for the lock
		return fresh, nil
	default:
	}
	deadline := time.Now().Add(mediaRefreshTimeout + mediaRefreshGrace)
	// The re-fetched page is history Swift did not ask for: only its media URLs are used.
	s.index.muteHistory(ref.ThreadKey, deadline)
	b.refreshes.collectUntil(deadline)
	ts := ref.TimestampMS
	if ts <= 0 {
		ts = time.Now().UnixMilli()
	}
	tbl, err := s.ls.ExecuteTasks(ctx, &socket.FetchMessagesTask{
		ThreadKey:            ref.ThreadKey,
		Direction:            0,
		ReferenceTimestampMs: ts + 1,
		ReferenceMessageId:   ref.MessageID,
		SyncGroup:            1,
		Cursor:               s.ls.Cursor(1),
	})
	if err != nil {
		return "", err
	}
	urls := tableMediaURLs(tbl)
	b.refreshes.remember(urls)
	if fresh := urls[ref.FBID]; fresh != "" {
		return fresh, nil
	}
	select {
	case fresh := <-ch:
		return fresh, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// mediaRefreshes serializes URL refreshes per thread and keeps the URLs they returned for a while.
// The zero value is ready to use.
type mediaRefreshes struct {
	mu          sync.Mutex
	locks       map[int64]*threadLock
	urls        map[string]refreshedURL
	order       []string
	collectTill time.Time // socket tables feed urls until then
}

type threadLock struct {
	ch   chan struct{}
	refs int
}

type refreshedURL struct {
	url string
	at  time.Time
}

// lock takes the refresh lock of thread key, giving up when ctx ends.
func (r *mediaRefreshes) lock(ctx context.Context, key int64) (func(), error) {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[int64]*threadLock{}
	}
	l := r.locks[key]
	if l == nil {
		l = &threadLock{ch: make(chan struct{}, 1)}
		r.locks[key] = l
	}
	l.refs++
	r.mu.Unlock()
	release := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if l.refs--; l.refs == 0 {
			delete(r.locks, key)
		}
	}
	select {
	case l.ch <- struct{}{}:
		return func() { <-l.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func (r *mediaRefreshes) collectUntil(t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.After(r.collectTill) {
		r.collectTill = t
	}
}

// observe keeps the media URLs of a socket table while a refresh may be answered over the socket.
func (r *mediaRefreshes) observe(tbl *table.LSTable) {
	r.mu.Lock()
	active := time.Now().Before(r.collectTill)
	r.mu.Unlock()
	if active && tbl != nil {
		r.remember(tableMediaURLs(tbl))
	}
}

func (r *mediaRefreshes) remember(urls map[string]string) {
	if len(urls) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.urls == nil {
		r.urls = map[string]refreshedURL{}
	}
	now := time.Now()
	for fbid, u := range urls {
		if _, ok := r.urls[fbid]; !ok {
			r.order = append(r.order, fbid)
		}
		r.urls[fbid] = refreshedURL{url: u, at: now}
	}
	for len(r.order) > refreshedURLsCapacity {
		delete(r.urls, r.order[0])
		r.order = r.order[1:]
	}
}

// lookup returns a recently refreshed URL for fbid that differs from the stale one.
func (r *mediaRefreshes) lookup(fbid, stale string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.urls[fbid]
	if !ok || u.url == stale || time.Since(u.at) > refreshedURLTTL {
		return ""
	}
	return u.url
}

// tableMediaURLs maps attachment fbids in tbl to their full URL (the preview URL when there is none).
func tableMediaURLs(tbl *table.LSTable) map[string]string {
	out := map[string]string{}
	if tbl == nil {
		return out
	}
	add := func(fbid, playable, preview string) {
		if fbid == "" {
			return
		}
		if playable != "" {
			out[fbid] = playable
		} else if preview != "" {
			out[fbid] = preview
		}
	}
	for _, a := range tbl.LSInsertBlobAttachment {
		add(a.AttachmentFbid, a.PlayableUrl, a.PreviewUrl)
	}
	for _, a := range tbl.LSInsertAttachment {
		add(a.AttachmentFbid, a.PlayableUrl, a.PreviewUrl)
	}
	for _, a := range tbl.LSInsertStickerAttachment {
		add(a.AttachmentFbid, a.PlayableUrl, a.PreviewUrl)
	}
	return out
}

// limitedFile is the whatsmeow.File of an E2EE download, capped at max bytes. It deliberately has
// no ReadFrom, so io.Copy goes through Write and the cap.
type limitedFile struct {
	f   *os.File
	max int64
}

func (l *limitedFile) Read(p []byte) (int, error)              { return l.f.Read(p) }
func (l *limitedFile) ReadAt(p []byte, off int64) (int, error) { return l.f.ReadAt(p, off) }
func (l *limitedFile) Seek(off int64, whence int) (int64, error) {
	return l.f.Seek(off, whence)
}
func (l *limitedFile) Stat() (os.FileInfo, error) { return l.f.Stat() }
func (l *limitedFile) Write(p []byte) (int, error) {
	pos, err := l.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if pos+int64(len(p)) > l.max {
		return 0, errMediaTooLarge
	}
	return l.f.Write(p)
}
func (l *limitedFile) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > l.max {
		return 0, errMediaTooLarge
	}
	return l.f.WriteAt(p, off)
}
func (l *limitedFile) Truncate(size int64) error {
	if size > l.max {
		return errMediaTooLarge
	}
	return l.f.Truncate(size)
}
