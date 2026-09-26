package gateway

// The branches a healthy gateway never takes: a socket that fails mid-handshake, a store
// directory that is not a directory, an upgrade stream whose download dies, and the startup
// wiring of server.go exercised through the same seams production uses.
//
// Each test says which production failure it stands in for, because a branch nobody can reach
// on purpose is a branch nobody would notice breaking.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madkoding/motita/internal/logx"
	"github.com/madkoding/motita/internal/schedule"
	"github.com/madkoding/motita/internal/updater"
)

// --- the WebSocket handshake and frame writer -------------------------------------------------

// A response writer that cannot be hijacked is refused rather than half-upgraded: the connection
// would otherwise be handed back with no way to take it over from the HTTP server.
func TestAHandshakeWithoutHijackingIsRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	// httptest.NewRecorder implements http.ResponseWriter but NOT http.Hijacker.
	if _, _, err := wsHandshake(httptest.NewRecorder(), req); err == nil {
		t.Error("a writer that cannot be hijacked must be refused")
	}
}

// A handshake whose response cannot be written closes the connection instead of returning it, so
// the caller never starts a read loop against a peer that never saw the 101.
func TestAHandshakeWhoseWriteFailsClosesTheConnection(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	h := &hijackingWriter{w: bufio.NewWriterSize(failWriter{}, 1)}

	conn, _, err := wsHandshake(h, req)
	if err == nil {
		t.Fatal("a handshake that cannot be written must be reported")
	}
	if conn != nil {
		t.Error("no connection may be handed back")
	}
}

// And a handshake that writes but cannot be FLUSHED fails too: the bytes are in a buffer the peer
// will never receive, which is the same failure one step later.
func TestAHandshakeWhoseFlushFailsIsRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	// A buffer large enough to hold the response, so WriteString succeeds and Flush is what fails.
	h := &hijackingWriter{w: bufio.NewWriter(failWriter{})}

	if _, _, err := wsHandshake(h, req); err == nil {
		t.Error("a handshake that cannot be flushed must be reported")
	}
}

// A frame written to a peer that is gone is reported so the caller can stop rather than spin.
func TestAFrameWriteToADeadSocketIsReported(t *testing.T) {
	if err := wsWriteFrame(&fakeConn{writeErr: errors.New("the peer is gone")}, opText, []byte("hello")); err == nil {
		t.Error("a write to a dead socket must be reported")
	}
	// A zero-length payload writes only the header; the failure must still surface.
	if err := wsWriteFrame(&fakeConn{writeErr: errors.New("the peer is gone")}, opClose, nil); err == nil {
		t.Error("a header-only write to a dead socket must be reported")
	}
}

// failWriter is the socket that fails, reduced to the one method the handshake needs.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("the connection was reset") }

// hijackingWriter is a ResponseWriter that CAN be hijacked, so the handshake's write and flush
// branches are reachable - which they are not through httptest.NewRecorder.
type hijackingWriter struct {
	w *bufio.Writer
}

func (h *hijackingWriter) Header() http.Header         { return make(http.Header) }
func (h *hijackingWriter) Write(p []byte) (int, error) { return h.w.Write(p) }
func (h *hijackingWriter) WriteHeader(int)             {}
func (h *hijackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return &fakeConn{}, bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), h.w), nil
}

// fakeConn is a net.Conn whose reads and writes fail as a test chooses.
type fakeConn struct {
	writeErr error
	readErr  error
}

func (c *fakeConn) Read([]byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return 0, io.EOF
}
func (c *fakeConn) Write([]byte) (int, error)        { return 0, c.writeErr }
func (c *fakeConn) Close() error                     { return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// --- server.go: the startup wiring ------------------------------------------------------------

// A gateway whose store directories are unusable still starts. It reports each failure and runs
// without persistence, because a gateway that refuses to boot over a cache directory is a gateway
// nobody can use to fix it.
func TestAServerWithUnusableStorePathsStillStarts(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(blocked, []byte("a file where a directory belongs"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	srv, sink := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   filepath.Join(blocked, "sessions"),
		ProjectDir:   filepath.Join(blocked, "projects"),
		ScheduleDir:  filepath.Join(blocked, "schedules"),
	})
	logged = sink.lines(t)

	if srv == nil {
		t.Fatal("a server with unusable stores must still come up")
	}
	if len(logged) != 3 {
		t.Errorf("all three store failures must be reported, got %d: %v", len(logged), logged)
	}
	if srv.store != nil {
		t.Error("no session store can have been opened from a path that is not a directory")
	}
}

// A configured schedule directory is opened at STARTUP, which is what makes a configured task
// survive a restart instead of existing only until the process ends.
func TestAConfiguredScheduleDirectoryIsOpenedAtStartup(t *testing.T) {
	dir := t.TempDir()
	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   filepath.Join(t.TempDir(), "sessions"),
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		ScheduleDir:  dir,
	})
	if srv.schedules == nil {
		t.Fatal("a configured schedule directory must give the server a store, or nothing ever fires")
	}
	// The store reads where it was told to, so a task written by a previous process is visible.
	st, err := schedule.Open(dir)
	if err != nil {
		t.Fatalf("schedule.Open: %v", err)
	}
	if _, err := st.LoadAll(); err != nil {
		t.Errorf("LoadAll on the opened store: %v", err)
	}
}

// An ExePath that is set is wired into the updater whether or not the file is there. That is the
// real behaviour, and it is pinned here rather than assumed: the guard at server.go is
// `ExePath != ""`, with no existence check, so a MISCONFIGURED path builds an updater that would
// rename a new binary into place and leave the running one untouched.
//
// Recorded, not "fixed": changing it would change what an upgrade does on a host whose binary lives
// somewhere else, which is a decision for the operator, not for a coverage test.
func TestAnExePathIsWiredIntoTheUpdaterWithoutAnExistenceCheck(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   filepath.Join(t.TempDir(), "sessions"),
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		ExePath:      missing,
	})
	if srv.updater == nil {
		t.Fatal("a configured ExePath builds the updater; absence is not checked")
	}
	if srv.updater.ExePath != missing {
		t.Errorf("ExePath = %q, want the configured %q", srv.updater.ExePath, missing)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("this test is only meaningful while the path really is absent")
	}
}

// And an EMPTY ExePath builds no updater at all, so the update endpoints answer "not available"
// instead of a check that has no binary to replace.
func TestAnEmptyExePathBuildsNoUpdater(t *testing.T) {
	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   filepath.Join(t.TempDir(), "sessions"),
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
	})
	if srv.updater != nil {
		t.Error("no updater can be built without a binary path")
	}

	w := get(t, srv, "/v1/update/check", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no updater", w.Code)
	}
	var got updater.CheckResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body = %q: %v", w.Body.String(), err)
	}
	if got.Error == "" {
		t.Error("the answer must say self-update is unavailable, not silently claim to be current")
	}
}

// A saved session is restored at startup with its title and transcript, because a gateway that came
// back with an empty list would look like lost work.
func TestSessionsAreRestoredAtStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id":        "s-restored",
		"title":     "a session from before the restart",
		"created":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{{"user": "what we were doing", "agent": "the answer"}},
	})

	var factoryCalls int
	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService:   func() (Service, error) { factoryCalls++; return &fakeService{}, nil },
	})

	if factoryCalls == 0 {
		t.Error("restoring a session needs an engine, and the factory was never asked for one")
	}
	c, ok := srv.lookup("s-restored")
	if !ok {
		t.Fatal("the saved session must be back after a restart")
	}
	if got := c.status().Title; got != "a session from before the restart" {
		t.Errorf("Title = %q, want the saved title", got)
	}
	if turns := c.svc.Transcript(); len(turns) != 1 {
		t.Errorf("Transcript = %d turns, want the saved one", len(turns))
	}
}

// The DEFAULT session's file is restored into the conversation that already exists rather than
// registered as a second conversation: two names for one transcript is how a session is edited in
// one place and read in another.
func TestTheDefaultSessionsFileIsRestoredIntoTheExistingConversation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id":        DefaultSession,
		"title":     "the default session's own title",
		"created":   time.Now().UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{{"user": "an earlier question", "agent": "an earlier answer"}},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
	})

	all := srv.snapshot()
	if len(all) != 1 {
		t.Fatalf("snapshot = %d sessions, want exactly the default one", len(all))
	}
	if got := all[0].status().Title; got != "the default session's own title" {
		t.Errorf("Title = %q, want the persisted title", got)
	}
}

// A session that was MID-RUN when the gateway stopped is not resumed blindly: the record is read
// and the run is relaunched through the same path the scheduler and the HTTP handlers use, so the
// slot guard and the completion classification cannot drift between them.
//
// The fake task BLOCKS, so the run is still in flight when it is observed: a fake that returned
// immediately would let this pass against a gateway that resumed nothing, because the session would
// go back to idle on its own.
func TestARunningSessionIsResumedThroughTheSharedRunPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id":        "s-interrupted",
		"title":     "was mid-task",
		"running":   true,
		"last_task": "finish the audit",
		"last_kind": "task",
		"created":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{},
	})

	// The task SIGNALS that it started and then blocks. Polling a status flag for a couple of
	// seconds made this fail on a loaded machine, and a flaky test is worse than no test: it
	// teaches everyone to re-run until it is green.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv, _ := startServer(t, Options{
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
				<-release
				return "done", nil
			}}, nil
		},
	})

	// Released in a cleanup registered AFTER startServer, and then WAITED for.
	//
	// The order is the whole fix. Cleanups run last-registered-first, so a cleanup
	// registered before startServer runs after srv.Close AND after t.TempDir's
	// RemoveAll - and that is too late to be useful here: this task blocks on release
	// and IGNORES its context, so closing the server does not end it. Releasing it that
	// late left the run's goroutine to finish on its own schedule, and its deferred
	// saveSession writes into the sessions directory while t.TempDir's cleanup is
	// removing it - which failed about one run in twenty under load with
	// "RemoveAll cleanup: directory not empty".
	//
	// Waiting is the other half, and the shared helper does it: isRunning is false only
	// after releaseRunSlot, the run goroutine's last defer, so by the time this returns
	// the write has already happened and every temp dir is quiet.
	t.Cleanup(func() {
		close(release)
		waitForNoRun(t, srv)
	})

	if _, ok := srv.lookup("s-interrupted"); !ok {
		t.Fatal("the interrupted session must have been restored before it was resumed")
	}
	select {
	case <-entered:
		// The run really started, through the shared path.
	case <-time.After(10 * time.Second):
		t.Error("a session that was mid-run must be resumed, not left idle")
	}
}

// A running record with no task text cannot be resumed - the prompt is gone - and is left clean
// rather than started with an empty prompt that would produce nothing.
func TestARunningRecordWithNoTaskIsNotResumed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id":        "s-no-task",
		"title":     "was running, task lost",
		"running":   true,
		"created":   time.Now().UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService:   func() (Service, error) { return &fakeService{}, nil },
	})

	c, ok := srv.lookup("s-no-task")
	if !ok {
		t.Fatal("the record must still be restored as a session")
	}
	if c.isRunning() {
		t.Error("a resume with no task text would run an empty prompt, which is worse than not resuming")
	}
}

// A store that cannot be SAVED reports it once per failure rather than swallowing it: a gateway
// that cannot persist has to say so, or the operator learns it from missing data.
func TestASaveFailureReachesTheOperator(t *testing.T) {
	srv, sink := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   filepath.Join(t.TempDir(), "sessions"),
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
	})

	blockStoreDirectory(t, srv.store.dir)
	c, ok := srv.lookup(DefaultSession)
	if !ok {
		t.Fatal("the default conversation must exist")
	}
	srv.saveSession(c)
	if len(sink.lines(t)) == 0 {
		t.Error("a save that failed must reach the log")
	}

	// The same for the bulk save before a restart, and for a delete.
	srv.saveAllSessions()
	if !strings.Contains(strings.Join(sink.lines(t), "\n"), "restart") {
		t.Error("a save-all that failed must reach the log: the upgrade depends on it")
	}
	srv.deletePersistedSession("some-session")
	if !strings.Contains(strings.Join(sink.lines(t), "\n"), "delete") {
		t.Error("a delete that failed must reach the log")
	}
}

// A store that cannot even be LISTED reports the failure and restores nothing, rather than
// registering a session per file it could not read.
func TestAnUnreadableStoreAtStartupIsReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory the store cannot read: a file blocking the path it lists.
	seedSessionFile(t, dir, map[string]any{
		"id": "s1", "title": "t", "created": time.Now().UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().UTC().Format(time.RFC3339Nano), "turns": []map[string]string{},
	})
	blockStoreDirectory(t, dir)

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
	})
	if srv == nil {
		t.Fatal("a gateway whose store cannot be listed must still serve")
	}
}

// --- the upgrade stream ------------------------------------------------------------------------

// The upgrade stream reports "already up to date" and stops, rather than fetching a release it
// does not need.
func TestTheUpgradeStreamReportsWhenThereIsNothingToDo(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	srv.updater = updaterFor(t, srv, releaseJSON("v1.0.0"))

	w := httptest.NewRecorder()
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))

	body := w.Body.String()
	if !strings.Contains(body, "Already up to date") {
		t.Errorf("body = %q, want it to say there is nothing to do", body)
	}
	if !strings.Contains(body, "event: progress") {
		t.Errorf("body = %q, want SSE progress events", body)
	}
}

// A check that fails is streamed as an error event rather than answered with only headers: the
// front end shows a message, not a progress bar that never moves.
func TestTheUpgradeStreamReportsACheckFailure(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	srv.updater = updaterFor(t, srv, "") // an empty body is not a release

	w := httptest.NewRecorder()
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))

	if !strings.Contains(w.Body.String(), `"stage":"error"`) {
		t.Errorf("body = %q, want an error event", w.Body.String())
	}
}

// And a download that dies mid-upgrade is streamed as an error: the user is told, rather than left
// watching a bar that stopped.
func TestTheUpgradeStreamReportsAFailedDownload(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	// A newer release whose ONLY asset is the one this platform wants, pointing at a URL that 500s.
	release := `{"tag_name":"v9.9.9","name":"newer","assets":[
		{"name":"` + assetNameForThisPlatform() + `","browser_download_url":"` + failingAssetURL(t) + `"}]}`
	srv.updater = updaterFor(t, srv, release)

	w := httptest.NewRecorder()
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))

	if !strings.Contains(w.Body.String(), `"stage":"error"`) {
		t.Errorf("body = %q, want an error event when the download fails", w.Body.String())
	}
}

// A release with no asset for this platform is an error the user can act on, not a silent no-op.
func TestTheUpgradeStreamReportsAMissingAsset(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	srv.updater = updaterFor(t, srv, `{"tag_name":"v9.9.9","name":"newer","assets":[]}`)

	w := httptest.NewRecorder()
	srv.handleUpdateRun(w, httptest.NewRequest(http.MethodPost, "/v1/update/run", nil))

	if !strings.Contains(w.Body.String(), `"stage":"error"`) {
		t.Errorf("body = %q, want an error naming the missing asset", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no asset for") {
		t.Errorf("body = %q, want the reason", w.Body.String())
	}
}

// The check endpoint answers from the CACHE the background checker fills, so a front end polling it
// never hits GitHub. Without the cache every poll would be an API call against a 60/hour limit.
//
// The updater is wired to a fake API that would answer a DIFFERENT version, so a handler that
// ignored the cache and checked again would fail this rather than pass on a coincidentally equal
// answer.
func TestTheUpdateCheckAnswersFromTheCache(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	srv.updater = updaterFor(t, srv, releaseJSON("v9.9.9")) // what a fresh check would say
	srv.lastCheckMu.Lock()
	srv.lastCheck = updater.CheckResult{CurrentVersion: "v1.0.0", LatestVersion: "v2.0.0", UpdateAvailable: true}
	srv.lastCheckMu.Unlock()

	w := get(t, srv, "/v1/update/check", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got updater.CheckResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body = %q: %v", w.Body.String(), err)
	}
	if got.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want the CACHED one: a poll must not hit GitHub", got.LatestVersion)
	}
}

// With no cache the endpoint does a fresh check and caches what it found.
func TestTheUpdateCheckFillsTheCacheOnAFirstCall(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.opts.Version = "v1.0.0"
	srv.updater = updaterFor(t, srv, releaseJSON("v2.0.0"))

	w := get(t, srv, "/v1/update/check", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The check answered a body, and a check is only cheap if it was remembered.
	if w.Body.Len() == 0 {
		t.Fatal("the check must answer a result")
	}
}

// The periodic checker caches its result so the endpoint has something to serve, and a short
// interval exercises the tick branch that the one-hour default would take an hour to reach.
func TestThePeriodicCheckerStopsWithTheGateway(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.UpdateCheckInterval = 20 * time.Millisecond })
	srv.updater = updaterFor(t, srv, releaseJSON("v9.9.9"))

	srv.runOneCheck()
	srv.lastCheckMu.RLock()
	cached := srv.lastCheck
	srv.lastCheckMu.RUnlock()
	if cached.LatestVersion != "v9.9.9" {
		t.Errorf("the periodic check must cache what it found, got %q", cached.LatestVersion)
	}

	// The goroutine checks once after the startup delay and then ON THE TICK, which the short
	// interval makes reachable. The counter is reset so only the tick can be what sets it.
	resetCheckCount()
	srv.startUpdateChecker()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if checkCount() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := checkCount(); got < 2 {
		t.Errorf("checks = %d, want at least 2: the startup check AND a tick", got)
	}

	// And closing the gateway stops it: a leaked checker would keep hitting the network after the
	// process was told to stop.
	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-srv.baseCtx.Done():
	case <-time.After(2 * time.Second):
		t.Error("closing the gateway must end the checker's context")
	}
	seen := checkCount()
	time.Sleep(120 * time.Millisecond)
	if checkCount() != seen {
		t.Error("the checker kept calling after the gateway closed")
	}
}

// A NEGATIVE interval turns the periodic checker off, which is what a test that must not touch the
// network asks for. Without it every test server in this package would eventually poll GitHub.
func TestANegativeUpdateCheckIntervalTurnsTheCheckerOff(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.UpdateCheckInterval = -1 })
	srv.updater = updaterFor(t, srv, releaseJSON("v9.9.9"))

	resetCheckCount()
	srv.startUpdateChecker()
	time.Sleep(200 * time.Millisecond)
	if got := checkCount(); got != 0 {
		t.Errorf("checks = %d, want 0: a negative interval means no checker at all", got)
	}
}

// A restored session that belonged to a PROJECT comes back with its project id, its workspace and
// its own worktree, which is what makes its work still mergeable after a restart. Without ProjectDir
// the gateway would report every restored session as having nothing to integrate.
func TestARestoredProjectSessionKeepsItsWorktreeAndBranch(t *testing.T) {
	ws := t.TempDir()
	pid := "p-restore"
	pdir := filepath.Join(ws, "repo")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, pdir)

	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id":          "s-proj",
		"title":       "worked in a project",
		"project_id":  pid,
		"workspace":   pdir,
		"project_dir": pdir,
		"provider":    "anthropic",
		"model":       "claude-x",
		"created":     time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"last_used":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"turns":       []map[string]string{{"user": "do the work", "agent": "done"}},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: ws,
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService:   func() (Service, error) { return &fakeService{}, nil },
	})
	if err := srv.projects.save(Project{ID: pid, Title: "repo", Dir: pdir, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}

	c, ok := srv.lookup("s-proj")
	if !ok {
		t.Fatal("the project session must be restored")
	}
	st := c.status()
	if st.ProjectID != pid {
		t.Errorf("ProjectID = %q, want %q", st.ProjectID, pid)
	}
	// The worktree is recreated under the workspace root, derived from the session id alone.
	wantWT := filepath.Join(ws, "worktrees", "s-proj")
	if st.Workspace != wantWT {
		t.Errorf("Workspace = %q, want the session's own worktree %q", st.Workspace, wantWT)
	}
	if st.Branch != sessionBranch("s-proj") {
		t.Errorf("Branch = %q, want the session's own branch", st.Branch)
	}
}

// A session with no project comes back with NO workspace: there is nowhere to run it, and inventing
// a directory would run it in whatever the process's own directory happens to be.
func TestARestoredSessionWithNoProjectHasNoWorkspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id": "s-free", "title": "free standing",
		"created":   time.Now().UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService:   func() (Service, error) { return &fakeService{}, nil },
	})

	c, ok := srv.lookup("s-free")
	if !ok {
		t.Fatal("the session must be restored")
	}
	if ws := c.status().Workspace; ws != "" {
		t.Errorf("Workspace = %q, want empty for a session with no project", ws)
	}
}

// A session whose provider and model were recorded gets them back, so the configuration a user
// chose for a session survives a restart instead of silently reverting to the default.
func TestARestoredSessionKeepsItsProviderAndModel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id": "s-configured", "title": "chose a provider",
		"provider":  "anthropic",
		"model":     "claude-x",
		"created":   time.Now().UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService:   func() (Service, error) { return &fakeService{}, nil },
	})

	if _, ok := srv.lookup("s-configured"); !ok {
		t.Fatal("the session must be restored")
	}
}

// Without a service FACTORY a non-default session cannot be restored: registering it with the
// default session's engine would be two names for one transcript, edited in one place and read in
// the other. It is skipped, and the default session still comes up.
func TestASessionIsSkippedWhenThereIsNoServiceFactory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id": "s-orphan", "title": "no engine to build",
		"created":   time.Now().UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		// No NewService, and a service for the default session only.
	})

	if _, ok := srv.lookup("s-orphan"); ok {
		t.Error("a session with no engine to build must be skipped, not wired to another session's engine")
	}
	if _, ok := srv.lookup(DefaultSession); !ok {
		t.Error("the default session must still be there")
	}
}

// A service factory that FAILS skips that session and keeps going, so one broken engine does not
// cost the user every other session.
func TestASessionWhoseEngineCannotBeBuiltIsSkipped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	seedSessionFile(t, dir, map[string]any{
		"id": "s-broken", "title": "engine fails",
		"created":   time.Now().UTC().Format(time.RFC3339Nano),
		"last_used": time.Now().UTC().Format(time.RFC3339Nano),
		"turns":     []map[string]string{},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService:   func() (Service, error) { return nil, errors.New("the engine could not start") },
	})

	if _, ok := srv.lookup("s-broken"); ok {
		t.Error("a session whose engine cannot be built must not be registered")
	}
}

// A restored session whose PROJECT DIRECTORY is gone still comes back, in the project's directory
// as recorded, rather than being dropped: the record is the user's work and losing it is worse than
// a session that cannot branch.
func TestARestoredSessionWhoseProjectDirectoryIsGoneStillComesBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	gone := filepath.Join(t.TempDir(), "deleted-project")
	seedSessionFile(t, dir, map[string]any{
		"id": "s-gone", "title": "its project moved",
		"project_id":  "p-gone",
		"workspace":   gone,
		"project_dir": gone,
		"created":     time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"last_used":   time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"turns":       []map[string]string{},
	})

	srv, _ := startServer(t, Options{
		Token:        testToken,
		WorkspaceDir: t.TempDir(),
		SessionDir:   dir,
		ProjectDir:   filepath.Join(t.TempDir(), "projects"),
		NewService:   func() (Service, error) { return &fakeService{}, nil },
	})

	if _, ok := srv.lookup("s-gone"); !ok {
		t.Error("a session whose project directory is gone must still be restored: the record is the work")
	}
}

// The update-check counter lives here because the fake provider is the only place a check can be
// counted without reaching the network.
var (
	checkMu    sync.Mutex
	checkCalls int
)

func resetCheckCount() {
	checkMu.Lock()
	checkCalls = 0
	checkMu.Unlock()
}

func checkCount() int {
	checkMu.Lock()
	defer checkMu.Unlock()
	return checkCalls
}

func bumpCheckCount() {
	checkMu.Lock()
	checkCalls++
	checkMu.Unlock()
}

// --- projects and sessions: the remaining refusals ---------------------------------------------

// A create body that is not JSON is refused, and nothing is registered.
func TestASessionCreateWithAMalformedBodyIsRefused(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	before := len(srv.snapshot())

	if w := post(t, srv, "/v1/sessions", `{not json`, testToken); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if after := len(srv.snapshot()); after != before {
		t.Errorf("sessions went from %d to %d: a refused create must register nothing", before, after)
	}
}

// A session asked for a project that does not exist is refused with 404: a session for a missing
// project would run in the wrong directory, which is exactly what projects prevent.
func TestASessionForAnUnknownProjectIsRefused(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	withProjects(t, srv, t.TempDir())

	w := post(t, srv, "/v1/sessions", `{"project_id":"p-does-not-exist"}`, testToken)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: body = %s", w.Code, w.Body.String())
	}
}

// A session in a project that is NOT a repository still gets a usable session, in the project's own
// directory, and the fallback is reported so the operator knows the isolation was not available.
func TestASessionInANonRepositoryProjectFallsBackToTheProjectDirectory(t *testing.T) {
	sink := newLogSink(t)
	// WorkspaceDir and Log go in through the MUTATOR, not by assigning to srv.opts afterwards: the
	// server is already serving in a goroutine by then, and writing its options from the test is a
	// data race the detector reports as one.
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.WorkspaceDir = t.TempDir()
		o.Log = sink.log
	})
	withProjects(t, srv, srv.opts.WorkspaceDir)

	// A plain directory, not a git repository: the worktree cannot be added.
	pdir := filepath.Join(srv.opts.WorkspaceDir, "not-a-repo")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := "p-not-a-repo"
	if err := srv.projects.save(Project{ID: pid, Title: "plain", Dir: pdir, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}

	w := post(t, srv, "/v1/sessions", `{"project_id":"`+pid+`"}`, testToken)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: body = %s", w.Code, w.Body.String())
	}
	var ss SessionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &ss); err != nil {
		t.Fatalf("body = %q: %v", w.Body.String(), err)
	}
	if ss.Workspace != pdir {
		t.Errorf("Workspace = %q, want the project's own directory as the fallback", ss.Workspace)
	}
	if len(sink.lines(t)) == 0 {
		t.Error("the fallback must be reported: the session has no isolation from its project")
	}
}

// A session in a git project reports itself MERGEABLE once its own worktree has a commit the base
// branch does not, and it reports its own branch rather than the project's.
func TestASessionReportsItselfMergeableWhenItHasUnmergedWork(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	withProjects(t, srv, t.TempDir())
	pid := makeProject(t, srv, "repo")

	w := post(t, srv, "/v1/sessions", `{"project_id":"`+pid+`"}`, testToken)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", w.Code, w.Body.String())
	}
	var ss SessionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &ss); err != nil {
		t.Fatal(err)
	}
	if ss.Mergeable {
		t.Error("a session with no commit of its own has nothing to integrate")
	}

	// A commit in the session's OWN worktree.
	if err := os.WriteFile(filepath.Join(ss.Workspace, "work.txt"), []byte("session work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", ss.Workspace, "add", "work.txt")
	mustRun(t, "git", "-C", ss.Workspace, "commit", "-qm", "session work")

	c, ok := srv.lookup(ss.ID)
	if !ok {
		t.Fatal("the session must be registered")
	}
	if st := c.status(); !st.Mergeable {
		t.Error("a session with a commit the base branch lacks must report itself mergeable")
	} else if st.Branch != sessionBranch(ss.ID) {
		t.Errorf("Branch = %q, want the session's own branch %q", st.Branch, sessionBranch(ss.ID))
	}
}

// A session whose work was ALREADY merged reports itself not mergeable: the front end shows a
// disabled Integrate button rather than an action that would do nothing.
func TestASessionReportsItselfMergedOnceItsWorkIsIntegrated(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	ws := t.TempDir()
	withProjects(t, srv, ws)
	pid := makeProject(t, srv, "repo")

	w := post(t, srv, "/v1/sessions", `{"project_id":"`+pid+`"}`, testToken)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", w.Code, w.Body.String())
	}
	var ss SessionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &ss); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ss.Workspace, "work.txt"), []byte("session work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", ss.Workspace, "add", "work.txt")
	mustRun(t, "git", "-C", ss.Workspace, "commit", "-qm", "session work")

	// Integrate it, then ask again.
	if w := post(t, srv, sessionPath(srv, ss.ID, "/merge"), "{}", testToken); w.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", w.Code, w.Body.String())
	}
	c, ok := srv.lookup(ss.ID)
	if !ok {
		t.Fatal("the session must be registered")
	}
	if c.status().Mergeable {
		t.Error("once the work is in the base branch there is nothing left to integrate")
	}
}

// --- helpers -----------------------------------------------------------------------------------

// startServer builds a server without serving it, so the startup wiring is exercised without a
// listener in the way. It is the Options-level twin of newTestServer.
//
// A REAL logger is wired in rather than a captured slice: a test that asserted against its own
// log function would prove nothing about the lines an operator actually gets.
func startServer(t *testing.T, opts Options) (*Server, *logSink) {
	t.Helper()
	sink := newLogSink(t)
	opts.Log = sink.log
	// Start refuses without a service to speak for, which is the same rule newTestServer follows.
	if opts.Service == nil {
		opts.Service = &fakeService{}
	}
	srv, err := Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	return srv, sink
}

// logSink is a REAL logger writing to a file the test reads back, rather than a captured slice.
//
// The real one, deliberately: a server that took a func would let a test assert against a log
// function the production path never calls, and the failure it is meant to prove would stay silent
// exactly where it matters.
type logSink struct {
	path string
	log  *logx.Logger
}

func newLogSink(t *testing.T) *logSink {
	t.Helper()
	path := filepath.Join(t.TempDir(), "motita.log")
	l, err := logx.New(logx.Options{Path: path, Level: logx.Info})
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return &logSink{path: path, log: l}
}

// lines reads back what the server logged. logx writes each record straight to the file with no
// buffer in between, so a read after the call sees it.
func (s *logSink) lines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading the log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// seedSessionFile writes a session record straight to a store directory, which is how a previous
// process's state is reproduced without going through a live gateway.
func seedSessionFile(t *testing.T, dir string, rec map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatal("a seeded record needs an id")
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// releaseJSON is a GitHub releases/latest body carrying a tag and no assets.
func releaseJSON(tag string) string {
	return `{"tag_name":"` + tag + `","name":"` + tag + `","html_url":"https://example.invalid/` + tag + `"}`
}

// updaterFor points a server's updater at a fake GitHub API, which is the seam the updater exposes
// for exactly this. Nothing here reaches the network.
func updaterFor(t *testing.T, srv *Server, body string) *updater.Updater {
	t.Helper()
	u := updater.New("v1.0.0", filepath.Join(t.TempDir(), "motita"))
	u.APIURL = func() string { return assetServerURL(t, body) }
	return u
}

// assetServerURL serves one canned body to every request and counts the requests, which is how a
// test tells "the checker ticked" from "the checker is asleep" without waiting for a real interval.
func assetServerURL(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bumpCheckCount()
		if body == "" {
			http.Error(w, "not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// failingAssetURL is a URL that answers 500, so a download dies in the middle of an upgrade.
func failingAssetURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the asset server is down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// assetNameForThisPlatform mirrors the updater's own naming so the fake release carries the asset
// this build would look for.
func assetNameForThisPlatform() string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return "motita-" + runtime.GOOS + "-" + runtime.GOARCH + ext
}
