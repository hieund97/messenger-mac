package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	e2eeMaxBackoff         = 5 * time.Minute
	e2eeLongDisconnectWarn = 2 * time.Minute
	e2eeRegisterTimeout    = 60 * time.Second
	e2eeCloseWait          = 5 * time.Second
)

// e2eeLongBackoff is used once E2EE is reported unavailable: retrying sooner would only repeat the
// same server rejection.
var e2eeLongBackoff = []time.Duration{5 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// e2eeConn is the E2EE (whatsmeow) side channel of one DGW session. E2EE failures never change the
// overall connection_state: auth_required comes only from the DGW side.
type e2eeConn struct {
	b      *MetaBackend
	owner  metaClient
	mc     *messagix.Client
	index  *ThreadIndex
	dbPath string
	log    zerolog.Logger
	ctx    context.Context

	// connectLock serializes connect() and close(), like e2eeConnectLock in client.go:553, so the
	// store is never closed under a running registration.
	connectLock sync.Mutex

	mu              sync.Mutex
	closed          bool
	started         bool
	container       *sqlstore.Container
	device          *store.Device
	cli             *whatsmeow.Client
	reset           bool // device was deleted and re-registered once in this session
	attempts        int
	longAttempts    int
	unavailableSent bool
	retry           *time.Timer
	longOffline     *time.Timer
}

func newE2EEConn(ctx context.Context, b *MetaBackend, owner metaClient, mc *messagix.Client, index *ThreadIndex, dbPath string, log zerolog.Logger) *e2eeConn {
	return &e2eeConn{b: b, owner: owner, mc: mc, index: index, dbPath: dbPath, log: log.With().Str("component", "e2ee").Logger(), ctx: ctx}
}

// start connects once per session; later calls are no-ops. Like client.go:472 it runs after the DGW
// socket reports connected, because registration needs the page configuration loaded by the inbox.
func (e *e2eeConn) start() {
	e.mu.Lock()
	if e.started || e.closed {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()
	go e.tryConnect()
}

func (e *e2eeConn) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

func (e *e2eeConn) tryConnect() {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error().Str("panic", fmt.Sprint(r)).Msg("Panic while connecting E2EE")
			e.scheduleRetry()
		}
	}()
	err := e.connect()
	if err == nil || e.isClosed() || e.ctx.Err() != nil {
		return
	}
	e.log.Error().Err(err).Msg("Failed to connect E2EE")
	if errors.Is(err, messagix.ErrICDCUnauthorized) {
		// client.go:503 does a full reconnect after an unauthorized device registration; a repeat
		// means reloading the page does not help.
		if e.b.noteICDCFailure() > 1 {
			e.unavailable("device registration rejected")
		} else if !e.b.scheduleE2EEFullReconnect(e.owner) {
			e.scheduleRetry()
		}
		return
	}
	e.scheduleRetry()
}

func (e *e2eeConn) openStore() (*sqlstore.Container, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, context.Canceled
	}
	if e.container != nil {
		return e.container, nil
	}
	c, err := sqlstore.New(e.ctx, "sqlite3", "file:"+e.dbPath+"?_foreign_keys=on", waLog.Zerolog(e.log))
	if err != nil {
		return nil, fmt.Errorf("open e2ee store: %w", err)
	}
	e.container = c
	return c, nil
}

func (e *e2eeConn) loadDevice(c *sqlstore.Container) (*store.Device, error) {
	devices, err := c.GetAllDevices(e.ctx)
	if err != nil {
		return nil, fmt.Errorf("load e2ee devices: %w", err)
	}
	self := strconv.FormatInt(e.index.self, 10)
	var found *store.Device
	for _, d := range devices {
		if d.ID != nil && d.ID.User == self && d.ID.Server == waTypes.MessengerServer {
			if found != nil {
				e.log.Warn().Msg("Multiple E2EE devices stored, using the first one")
				break
			}
			found = d
		}
	}
	return found, nil
}

// connect follows client.go:553-615: restore the stored device or register a new one, then connect.
func (e *e2eeConn) connect() error {
	e.connectLock.Lock()
	defer e.connectLock.Unlock()
	c, err := e.openStore()
	if err != nil {
		return err
	}
	e.mu.Lock()
	dev := e.device
	e.mu.Unlock()
	if dev == nil {
		if dev, err = e.loadDevice(c); err != nil {
			return err
		}
	}
	fresh := dev == nil
	if fresh {
		dev = c.NewDevice()
		if suggested := e.mc.MessengerLite.GetSuggestedDeviceID(); suggested != uuid.Nil {
			dev.FacebookUUID = suggested
		}
	}
	e.mc.SetDevice(dev)
	if fresh {
		// A half-finished registration would leave a device the server knows but we never saved, so
		// it is not cut short by the session ending; close() waits for it through connectLock.
		regCtx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), e2eeRegisterTimeout)
		err = e.mc.RegisterE2EE(regCtx, e.index.self)
		if err == nil {
			err = dev.Save(regCtx)
			if err != nil {
				err = fmt.Errorf("save e2ee device: %w", err)
			}
		} else {
			err = fmt.Errorf("register e2ee device: %w", err)
		}
		cancel()
		if err != nil {
			return err
		}
	}
	if e.isClosed() {
		return context.Canceled
	}
	cli, err := e.mc.PrepareE2EEClient()
	if err != nil {
		return fmt.Errorf("prepare e2ee client: %w", err)
	}
	// client.go:603-607: ack only after the handler returned true, and keep decrypted events that
	// could not be handled so a redelivery does not hit an advanced ratchet.
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool { return e.handle(cli, evt) })
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return context.Canceled
	}
	e.device, e.cli = dev, cli
	e.mu.Unlock()
	if err = cli.Connect(); err != nil {
		return fmt.Errorf("connect e2ee socket: %w", err)
	}
	return nil
}

func (e *e2eeConn) current(cli *whatsmeow.Client) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed && e.cli == cli
}

func (e *e2eeConn) emitError(code, message string) {
	if emit := e.b.sink(e.owner); emit != nil {
		emit(Event{Type: EventError, Data: ErrorEvent{Code: code, Message: message}})
	}
}

// handle returns false when an event could not be delivered, so with SynchronousAck the server sends
// it again later instead of it being lost.
func (e *e2eeConn) handle(cli *whatsmeow.Client, raw any) bool {
	if !e.current(cli) {
		return false
	}
	switch evt := raw.(type) {
	case *events.FBMessage:
		emit := e.b.sink(e.owner)
		if emit == nil {
			return false
		}
		e.index.ApplyWAMessage(evt, emit)
	case *events.UndecryptableMessage:
		// handlewhatsapp.go:51 hides messages the server asks to hide.
		if evt.DecryptFailMode == events.DecryptFailHide {
			return true
		}
		emit := e.b.sink(e.owner)
		if emit == nil {
			return false
		}
		emit(Event{Type: EventError, Data: ErrorEvent{Code: "undecryptable", Message: "an encrypted message could not be decrypted"}})
	case *events.Connected:
		e.b.e2eeHealthy()
		e.mu.Lock()
		e.attempts, e.longAttempts, e.unavailableSent = 0, 0, false
		if e.longOffline != nil {
			e.longOffline.Stop()
			e.longOffline = nil
		}
		e.mu.Unlock()
	case *events.Disconnected:
		e.mu.Lock()
		if e.longOffline == nil && !e.closed {
			e.longOffline = time.AfterFunc(e2eeLongDisconnectWarn, func() {
				e.log.Error().Msg("E2EE socket disconnected for a long time")
			})
		}
		e.mu.Unlock()
	case *events.CATRefreshError:
		// handlewhatsapp.go:118 reloads the page to get a fresh CAT.
		e.log.Error().Err(evt.Error).Msg("E2EE CAT refresh failed")
		e.fullReconnectOrRetry(cli)
	case *events.LoggedOut:
		// handlewhatsapp.go:139: a logout usually means the device was removed; re-register once.
		e.log.Error().Int("reason", int(evt.Reason)).Msg("E2EE device logged out")
		if !e.resetDevice(cli) {
			e.unavailableFrom(cli, "device logged out")
		}
	case *events.ConnectFailure:
		e.log.Error().Int("reason", int(evt.Reason)).Msg("E2EE connect failure")
		switch evt.Reason {
		case events.ConnectFailureNotFound, events.ConnectFailureClientUnknown:
			// handlewhatsapp.go:150: the server no longer knows this device.
			if !e.resetDevice(cli) {
				e.unavailableFrom(cli, "device rejected after re-registration")
			}
		case events.ConnectFailureGeneric:
			e.fullReconnectOrRetry(cli)
		default:
			e.retryFrom(cli, false)
		}
	case *events.ClientOutdated:
		e.log.Error().Msg("E2EE client outdated")
		e.unavailableFrom(cli, "client outdated")
	case *events.TemporaryBan:
		e.log.Error().Int("code", int(evt.Code)).Msg("E2EE temporary ban")
		e.unavailableFrom(cli, "temporarily banned")
	case *events.StreamReplaced:
		// Another client is using the same device keys; reconnecting would just kick it out again.
		e.log.Error().Msg("E2EE stream replaced")
		e.emitError("e2ee_stream_replaced", "another app is using this encrypted device")
	}
	return true
}

func (e *e2eeConn) fullReconnectOrRetry(cli *whatsmeow.Client) {
	if !e.b.scheduleE2EEFullReconnect(e.owner) {
		e.retryFrom(cli, false)
	}
}

// unavailable reports E2EE as down once per outage and retries with the long backoff.
func (e *e2eeConn) unavailable(reason string) {
	e.mu.Lock()
	send := !e.unavailableSent && !e.closed
	e.unavailableSent = true
	e.mu.Unlock()
	if send {
		e.emitError("e2ee_unavailable", reason)
	}
	e.retryFrom(nil, true)
}

func (e *e2eeConn) unavailableFrom(cli *whatsmeow.Client, reason string) {
	e.mu.Lock()
	if e.cli == cli {
		e.cli = nil
	}
	e.mu.Unlock()
	if cli != nil {
		go cli.Disconnect()
	}
	e.unavailable(reason)
}

// resetDevice deletes the local device and registers a new one, at most once per session.
func (e *e2eeConn) resetDevice(cli *whatsmeow.Client) bool {
	e.mu.Lock()
	if e.reset || e.closed || e.cli != cli {
		e.mu.Unlock()
		return false
	}
	e.reset = true
	dev := e.device
	e.cli, e.device = nil, nil
	e.mu.Unlock()
	go func() {
		if cli != nil {
			cli.Disconnect()
		}
		if dev != nil {
			delCtx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), e2eeRegisterTimeout)
			if err := dev.Delete(delCtx); err != nil {
				e.log.Error().Err(err).Msg("Failed to delete E2EE device")
			}
			cancel()
		}
		e.tryConnect()
	}()
	return true
}

func (e *e2eeConn) scheduleRetry() { e.retryFrom(nil, false) }

// retryFrom drops cli (if it is still the current client) and reconnects after a backoff: short
// exponential for transient failures, e2eeLongBackoff once E2EE was reported unavailable.
func (e *e2eeConn) retryFrom(cli *whatsmeow.Client, long bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.ctx.Err() != nil {
		return
	}
	var backoff time.Duration
	if long {
		backoff = e2eeLongBackoff[min(e.longAttempts, len(e2eeLongBackoff)-1)]
		e.longAttempts++
	} else {
		e.attempts++
		backoff = min(time.Duration(1<<min(e.attempts, 10))*time.Second, e2eeMaxBackoff)
	}
	if e.retry != nil {
		e.retry.Stop()
	}
	old := e.cli
	if cli == nil || old == cli {
		e.cli = nil
	} else {
		old = nil
	}
	e.retry = time.AfterFunc(backoff, func() {
		if old != nil {
			old.Disconnect()
		}
		if !e.isClosed() && e.ctx.Err() == nil {
			e.tryConnect()
		}
	})
}

// close disconnects and releases the store, after any running connect() finished. It must not be
// called from a whatsmeow event handler of this connection.
func (e *e2eeConn) close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	if e.retry != nil {
		e.retry.Stop()
	}
	if e.longOffline != nil {
		e.longOffline.Stop()
	}
	e.mu.Unlock()
	e.connectLock.Lock()
	defer e.connectLock.Unlock()
	e.mu.Lock()
	cli, container := e.cli, e.container
	e.cli, e.container = nil, nil
	e.mu.Unlock()
	if cli != nil {
		cli.Disconnect()
	}
	if container != nil {
		_ = container.Close()
	}
}

// closeAsync closes in the background and waits at most e2eeCloseWait, so a slow registration never
// blocks the command lock (b.op) for long.
func (e *e2eeConn) closeAsync() {
	done := make(chan struct{})
	go func() { e.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(e2eeCloseWait):
	}
}
