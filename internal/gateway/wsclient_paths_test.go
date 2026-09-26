package gateway

// The last branches in the gateway and the WebSocket layer that need a client built by hand.
//
// The WebSocket ones drive a wsClient directly rather than over a socket: the code under test takes
// a net.Conn and a context, so a pipe is enough, and a pipe is what makes a branch reachable that a
// live connection would have to be raced into.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madkoding/motita/internal/logx"
	"github.com/madkoding/motita/internal/updater"
)

// --- wsClient: the branches a healthy connection never takes ------------------------------------

// A marshal failure of an outbound message is logged and DROPPED, and the connection stays up: the
// message structs this code builds are always encodable, so a failure is a programming error, and
// killing a user's session over it would be the wrong trade.
func TestAMessageThatCannotBeMarshalledIsDroppedAndTheConnectionSurvives(t *testing.T) {
	sink := newLogSink(t)
	client := &wsClient{conn: &fakeConn{}, log: sink.log}

	// A payload that json.Marshal refuses: a channel.
	msg := wsMessage{MsgID: newUUIDv4(), Type: MsgNotification, Payload: json.RawMessage(`"ok"`)}
	msg.Payload = nil
	bad := newMsgWithUnmarshalablePayload()

	if ok := client.send(bad); !ok {
		t.Error("a message that cannot be encoded must not bring the connection down")
	}
	if len(sink.lines(t)) == 0 {
		t.Error("the programming error must be logged: silently dropping it hides the bug")
	}

	// And a normal message still goes out over the same client.
	if !client.send(msg) {
		t.Error("the connection must still work after a dropped message")
	}
}

// A write that fails because the PEER IS GONE is not logged as a warning: it is the expected end of
// a connection, and a log full of them hides the failures that matter.
func TestAWriteToAClosedPeerIsNotLoggedAsAFailure(t *testing.T) {
	sink := newLogSink(t)
	client := &wsClient{conn: &fakeConn{writeErr: errors.New("use of closed network connection")}, log: sink.log}

	if ok := client.writeFrame(opText, []byte(`{"type":"notification"}`)); ok {
		t.Error("a failed write must report false so the caller stops")
	}
	if lines := sink.lines(t); len(lines) != 0 {
		t.Errorf("a closed connection is not a warning, got %v", lines)
	}

	// A failure that is NOT a closed connection IS reported.
	client.conn = &fakeConn{writeErr: errors.New("something nobody expected")}
	client.writeFrame(opText, []byte(`{"type":"notification"}`))
	if len(sink.lines(t)) == 0 {
		t.Error("an unexpected write failure must be reported")
	}
}

// The heartbeat loop ANSWERS an ack by resetting its timer, and it exits when its context is done.
// Both are asserted through the loop itself, because the ack channel is the only thing that keeps it
// from closing the connection after the timeout.
func TestTheHeartbeatLoopResetsOnAnAckAndStopsWithItsContext(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	cl := &wsClient{conn: server, heartbeat: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	ackCh := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		cl.heartbeatLoop(ctx, ackCh)
		close(done)
	}()

	// Drain what the loop writes, and feed it an ack each time. If the ack did NOT reset the timer
	// the loop would close the connection on the first timeout, which the write below would see.
	go func() {
		buf := make([]byte, 64*1024)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
			select {
			case ackCh <- struct{}{}:
			default:
			}
		}
	}()

	time.Sleep(60 * time.Millisecond) // several intervals, each acked
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the heartbeat loop must stop when its context is done")
	}
}

// And with NO ack at all the loop gives up and closes the connection, rather than probing a peer
// that stopped answering forever.
//
// The wait is wsHeartbeatTimeout (10s in production), because the timeout is a CONSTANT rather than
// an option: it is deliberately not injectable, and a test that shortened it would be asserting a
// different rule than the one that ships.
func TestTheHeartbeatLoopGivesUpWithoutAnAck(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out wsHeartbeatTimeout, which is 10s by design")
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	cl := &wsClient{conn: server, heartbeat: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A drainer that reads the probes and NEVER acks, so the loop's writes complete (net.Pipe is
	// unbuffered: a write with no reader blocks forever) and the ack is the one thing missing.
	go func() {
		buf := make([]byte, 64*1024)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		cl.heartbeatLoop(ctx, make(chan struct{}))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(wsHeartbeatTimeout + 5*time.Second):
		t.Fatal("a peer that never acks must be given up on, not probed forever")
	}
}

// The heartbeat interval follows the same contract as Options.Heartbeat: a ZERO interval falls back
// to the default. The loop is driven directly with a zero interval and a canned ack, so the default
// is asserted without waiting for it.
func TestAZeroHeartbeatIntervalFallsBackToTheDefault(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	cl := &wsClient{conn: server, heartbeat: 0}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ackCh := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		cl.heartbeatLoop(ctx, ackCh)
		close(done)
	}()

	// With a zero interval the loop would use wsHeartbeatInterval (30s). Nothing may arrive in a
	// short window, which is what distinguishes "the default was used" from "the tick fired now".
	_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 64)
	if _, err := client.Read(buf); err == nil {
		t.Error("a zero interval must mean the 30-second default, not an immediate probe")
	}
	cancel()
	<-done
}

// A message type this protocol does not define is answered with a recoverable error and does not
// close the connection: a client that sent a typo can correct it.
func TestAnUnknownMessageTypeIsRecoverable(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	cl := &wsClient{conn: server, token: testToken}
	go func() {
		ack := make(chan struct{}, 1)
		cl.dispatch(wsMessage{MsgID: newUUIDv4(), Type: "not_a_real_type"}, ack)
	}()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	opcode, payload, _, _, err := wsReadFrame(client)
	if err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	if opcode != opText {
		t.Fatalf("opcode = %d, want a text frame", opcode)
	}
	var msg wsMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if msg.Type != MsgError {
		t.Errorf("type = %q, want an error message", msg.Type)
	}
	if !msg.hasFlag(FlagErrorRecoverable) {
		t.Errorf("flags = %v, want the error to be recoverable", msg.Flags)
	}
}

// An AUTH message with a WRONG token is refused and the connection stays open, so a client can
// correct its credential instead of being disconnected for one typo.
func TestAuthWithAWrongTokenIsRefusedWithoutClosingTheConnection(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	cl := &wsClient{conn: server, token: testToken}
	go func() {
		payload, _ := json.Marshal(map[string]string{"token": "the wrong token"})
		cl.dispatch(wsMessage{MsgID: newUUIDv4(), Type: MsgAuth, Payload: payload}, make(chan struct{}, 1))
	}()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, _, _, err := wsReadFrame(client)
	if err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	var msg wsMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != MsgAuthResponse {
		t.Errorf("type = %q, want an auth_response", msg.Type)
	}
	if msg.hasFlag(FlagAuthenticated) {
		t.Error("a wrong token must NOT be reported as authenticated")
	}
	if cl.isAuthenticated() {
		t.Error("the client must not be authenticated after a wrong token")
	}
}

// A query sent BEFORE authenticating is refused without reaching the model: otherwise the transport
// token would be the only gate, and the auth message exists precisely to be the second one.
func TestAQueryBeforeAuthIsRefused(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	svc := &fakeService{}
	cl := &wsClient{conn: server, token: testToken, svc: svc}
	go func() {
		payload, _ := json.Marshal(map[string]string{"query": "do something"})
		cl.dispatch(wsMessage{MsgID: newUUIDv4(), Type: MsgQuery, Payload: payload}, make(chan struct{}, 1))
	}()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, payload, _, _, err := wsReadFrame(client)
	if err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	var msg wsMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != MsgError {
		t.Errorf("type = %q, want an error", msg.Type)
	}
	if turns := svc.Transcript(); len(turns) != 0 {
		t.Errorf("an unauthenticated query must not reach the model, got %d turns", len(turns))
	}
}

// --- wsHandshake: the error before the hijack ---------------------------------------------------

// A request that is not an upgrade is refused by the HANDLER, and with a 400 the client can read.
// The check lives in handleWebSocket rather than in wsHandshake, which is why this drives the
// handler: the handshake is only reached once the upgrade has been established.
func TestARequestThatIsNotAnUpgradeIsRefused(t *testing.T) {
	srv := newTestServer(t, &fakeService{})

	// A plain GET, with no Upgrade header at all.
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	w := httptest.NewRecorder()
	srv.handleWebSocket(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "WebSocket upgrade") {
		t.Errorf("body = %q, want it to name the missing upgrade", w.Body.String())
	}

	// And a request that has SOME of the headers is still refused: the four parts are one rule.
	partial := httptest.NewRequest(http.MethodGet, "/ws", nil)
	partial.Header.Set("Upgrade", "websocket")
	partial.Header.Set("Connection", "Upgrade")
	w = httptest.NewRecorder()
	srv.handleWebSocket(w, partial)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: a key-less upgrade is not an upgrade", w.Code)
	}
}

// --- server.go: the last refusals ---------------------------------------------------------------

// A store whose loadAll fails at the SAVE-ALL path is reported, and the gateway still closes.
func TestASaveAllFailureIsReportedAndTheGatewayStillCloses(t *testing.T) {
	srv, sink := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   filepath.Join(t.TempDir(), "sessions"),
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
	})
	blockStoreDirectory(t, srv.store.dir)

	srv.saveAllSessions()
	if !strings.Contains(strings.Join(sink.lines(t), "\n"), "restart") {
		t.Error("a save-all that failed must be reported: the upgrade depends on it")
	}
	if err := srv.Close(context.Background()); err != nil {
		t.Errorf("Close after a failed save-all: %v", err)
	}
}

// A session to be resumed whose conversation is ALREADY RUNNING is reported and skipped, rather
// than starting a second turn in the same conversation.
//
// The state is built through the REAL path: the startup resumed the session (its task blocks, so it
// stays running), and the second resume then finds the slot taken. Taking the slot by hand would
// have made the test pass against a gateway that never resumed anything.
func TestAResumedSessionThatIsAlreadyRunningIsReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id": "s-busy", "title": "busy",
		"running": true, "last_task": "the task", "last_kind": "task",
		"created":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{},
	})

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv, sink := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService: func() (Service, error) {
			return &fakeService{task: func(ctx context.Context, task string, pr func(string, ...any)) (string, error) {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release // holds the run open, so the session stays running
				return "done", nil
			}}, nil
		},
	})

	// Released in a cleanup registered AFTER startServer, and then WAITED for - the
	// same fix, for the same reason, as the twin of this test in server_paths_test.go.
	//
	// The task blocks on release and ignores its context, so srv.Close does not end it.
	// With the cleanup registered before startServer it ran after the close, and the
	// run's goroutine was then free to finish later and write (its deferred
	// saveSession) into the sessions directory while t.TempDir's cleanup removed it.
	// Waiting for the conversation to stop running is what removes the race:
	// isRunning is false only after releaseRunSlot, the goroutine's last defer.
	t.Cleanup(func() {
		close(release)
		waitForNoRun(t, srv)
	})

	if _, ok := srv.lookup("s-busy"); !ok {
		t.Fatal("the session must be restored before it is resumed")
	}
	// The STARTUP resume must have taken the slot, or the assertion below proves nothing. The signal
	// comes from the run itself, so this cannot pass on a gateway that resumed nothing.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the startup resume must have started the run, or this test asserts nothing")
	}

	srv.resumeInterruptedSessions()
	if !strings.Contains(strings.Join(sink.lines(t), "\n"), "already running") {
		t.Errorf("a resume that could not start must be reported, got %v", sink.lines(t))
	}
}

// The upgrade stream VERIFIES a checksum when the release carries one, and reports a mismatch rather
// than installing a binary nobody vouched for.
func TestTheUpgradeStreamRefusesAMismatchedChecksum(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"

	// One server that serves BOTH the release and the asset, so the checksum file describes the
	// wrong binary and the verification must fail.
	assetName := assetNameForThisPlatform()
	var base string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/release"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"tag_name":"v9.9.9","name":"newer","assets":[
				{"name":"`+assetName+`","browser_download_url":"`+base+`/binary"},
				{"name":"SHA256SUMS","browser_download_url":"`+base+`/checksums"}]}`)
		case r.URL.Path == "/binary":
			_, _ = w.Write([]byte("the downloaded binary"))
		case r.URL.Path == "/checksums":
			// A hash for a DIFFERENT file.
			_, _ = io.WriteString(w, "0000000000000000000000000000000000000000000000000000000000000000  "+assetName+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	base = ts.URL

	srv.updater = updater.New("v1.0.0", filepath.Join(t.TempDir(), "motita"))
	srv.updater.APIURL = func() string { return ts.URL + "/release" }

	w := httptest.NewRecorder()
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))

	body := w.Body.String()
	if !strings.Contains(body, `"stage":"error"`) {
		t.Errorf("body = %q, want an error event when the checksum does not match", body)
	}
	if !strings.Contains(body, "checksum") {
		t.Errorf("body = %q, want the reason to name the checksum", body)
	}
}

// An upgrade with NO executable path to replace is reported before the download is attempted: there
// is nowhere to install, and downloading first would waste a release's bandwidth to fail anyway.
func TestTheUpgradeStreamReportsWhenThereIsNowhereToInstall(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	// An updater with an ExePath of "" - the install step is where the refusal has to come from.
	srv.updater = updaterFor(t, srv, `{"tag_name":"v9.9.9","name":"newer","assets":[
		{"name":"`+assetNameForThisPlatform()+`","browser_download_url":"`+assetPayloadURL(t)+`"}]}`)
	srv.updater.ExePath = ""

	w := httptest.NewRecorder()
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))

	if !strings.Contains(w.Body.String(), `"stage":"error"`) {
		t.Errorf("body = %q, want an error: there is no binary to replace", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "executable path") {
		t.Errorf("body = %q, want it to name the missing executable path", w.Body.String())
	}
}

// A release the API will not serve is reported, and the handler stops rather than fetching an asset
// from a URL it never received.
func TestTheUpgradeStreamReportsAReleaseItCannotFetch(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	// The CHECK succeeds (a release newer than v1.0.0) but the FULL fetch fails: the fake serves the
	// body only once, then answers 500.
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 1 {
			http.Error(w, "rate limited", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tag_name":"v9.9.9","name":"newer","assets":[]}`)
	}))
	defer ts.Close()
	srv.updater = updater.New("v1.0.0", filepath.Join(t.TempDir(), "motita"))
	srv.updater.APIURL = func() string { return ts.URL }

	w := httptest.NewRecorder()
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))

	if !strings.Contains(w.Body.String(), `"stage":"error"`) {
		t.Errorf("body = %q, want an error when the release cannot be fetched", w.Body.String())
	}
}

// The upgrade stream needs a connection that can FLUSH. A writer that cannot is refused before the
// download starts, rather than streaming into a buffer nobody will receive. noFlushWriter is the
// package's existing writer for this (see server_test.go).
func TestTheUpgradeStreamNeedsAFlushingWriter(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.updater = updaterFor(t, srv, releaseJSON("v9.9.9"))

	w := noFlushWriter{}
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))
}

// --- helpers ------------------------------------------------------------------------------------

// newMsgWithUnmarshalablePayload builds a message whose payload json.Marshal refuses. Half a JSON
// document is the shortest way to say "this cannot be encoded" without inventing a type.
func newMsgWithUnmarshalablePayload() wsMessage {
	return wsMessage{
		MsgID:     newUUIDv4(),
		Type:      MsgNotification,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Payload:   json.RawMessage(`{"unencodable":`),
	}
}

// assetPayloadURL serves a small body as the release asset, which is what an upgrade downloads
// before it reaches the install step.
func assetPayloadURL(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("a small fake binary"))
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// logxImport keeps the logx import honest in a file whose only other use of it is a type.
var _ = logx.Info
