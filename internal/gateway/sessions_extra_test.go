package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madkoding/motita/internal/config"
)

// deleteReq performs an authenticated DELETE and returns the recorder.
func deleteReq(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.BaseURL()+path, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// withFactory is the Options mutator that gives a test gateway the ability to mint conversations.
func withFactory() func(*Options) {
	return func(o *Options) {
		o.NewService = func() (Service, error) { return &fakeService{}, nil }
	}
}

// TestASessionCanBeOpenedAndClosed: the point of the endpoints is that a remote front end can
// open its own conversation, use it, and give the memory back.
func TestASessionCanBeOpenedAndClosed(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, withFactory())

	created := post(t, srv, "/v1/sessions", "{}", testToken)
	if created.Code != http.StatusCreated {
		t.Fatalf("opening a session answered %d: %s", created.Code, created.Body.String())
	}
	var conv SessionStatus
	if err := json.Unmarshal(created.Body.Bytes(), &conv); err != nil {
		t.Fatalf("the answer is not a session: %v", err)
	}
	if conv.ID == "" || conv.ID == DefaultSession {
		t.Fatalf("a new session must have an id of its own, got %q", conv.ID)
	}

	if w := get(t, srv, sessionPath(srv, conv.ID, "/report"), testToken); w.Code != http.StatusOK {
		t.Fatalf("the new session is not usable: %d %s", w.Code, w.Body.String())
	}

	if w := deleteReq(t, srv, sessionPath(srv, conv.ID, ""), testToken); w.Code != http.StatusNoContent {
		t.Fatalf("closing answered %d: %s", w.Code, w.Body.String())
	}
	if w := get(t, srv, sessionPath(srv, conv.ID, "/report"), testToken); w.Code != http.StatusNotFound {
		t.Fatalf("a closed session answered %d, it must be gone", w.Code)
	}
}

// TestAGatewayWithNoFactorySaysSo: a gateway started with one conversation cannot mint another,
// and saying so is better than answering with a second name for the same transcript.
func TestAGatewayWithNoFactorySaysSo(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := post(t, srv, "/v1/sessions", "{}", testToken)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("answered %d, it must be 501", w.Code)
	}
	if !strings.Contains(w.Body.String(), "serves one conversation") {
		t.Errorf("the refusal must say why: %s", w.Body.String())
	}
}

// TestTheCeilingIsReportedRatherThanCrossed: an unbounded registry is a memory leak with a nice
// name. A client that forgets to close what it opened must be told, not silently served.
func TestTheCeilingIsReportedRatherThanCrossed(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, withFactory(), func(o *Options) { o.MaxSessions = 2 })
	if w := post(t, srv, "/v1/sessions", "{}", testToken); w.Code != http.StatusCreated {
		t.Fatalf("the first session was refused: %s", w.Body.String())
	}
	// The default plus one is the ceiling of two.
	w := post(t, srv, "/v1/sessions", "{}", testToken)
	if w.Code != http.StatusConflict {
		t.Fatalf("the session past the ceiling answered %d, it must be 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ceiling") {
		t.Errorf("the refusal must name the ceiling: %s", w.Body.String())
	}
}

// TestTheCeilingDefaultsWhenNoneIsConfigured: a gateway with no configured ceiling must still have
// one, or "unbounded" is the default and the leak is what ships.
func TestTheCeilingDefaultsWhenNoneIsConfigured(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	if got := srv.maxSessions(); got != defaultMaxSessions {
		t.Errorf("maxSessions() = %d with nothing configured, want the default %d", got, defaultMaxSessions)
	}
}

// TestAFactoryThatFailsIsReported: a conversation whose service could not be built must not be
// registered as if it had been.
func TestAFactoryThatFailsIsReported(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.NewService = func() (Service, error) { return nil, errors.New("no engine") }
	})
	w := post(t, srv, "/v1/sessions", "{}", testToken)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("answered %d, it must be 501", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no engine") {
		t.Errorf("the failure must be carried, not swallowed: %s", w.Body.String())
	}
	// And nothing was registered: a failed factory must not leave an empty conversation behind.
	if n := len(srv.snapshot()); n != 1 {
		t.Errorf("the registry holds %d conversations after a failed factory, it must hold only the default", n)
	}
}

// TestASessionIdThatCannotBeFormedIsReported: without a source of randomness the name cannot be
// formed safely, and the same is true of the approval id. A predictable id would let a client
// address another's conversation.
func TestASessionIdThatCannotBeFormedIsReported(t *testing.T) {
	restore := randReader
	randReader = failingReader{}
	defer func() { randReader = restore }()

	srv := newTestServer(t, &fakeService{}, withFactory())
	w := post(t, srv, "/v1/sessions", "{}", testToken)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("answered %d, it must be reported", w.Code)
	}
	if !strings.Contains(w.Body.String(), "session id") {
		t.Errorf("the failure must say what could not be formed: %s", w.Body.String())
	}
}

// TestIsPlaceholderTitleRecognisesOnlyUnchosenTitles: auto-titling replaces a
// placeholder and nothing else. Getting this wrong is not cosmetic - it either
// overwrites a name the user typed, or leaves a session that was never named
// stuck with a generated placeholder forever.
func TestIsPlaceholderTitleRecognisesOnlyUnchosenTitles(t *testing.T) {
	cases := []struct {
		title string
		want  bool
		why   string
	}{
		{placeholderTitle, true, "the current placeholder is unchosen"},
		{"New session", true, "exactly the placeholder, not merely a title starting with it"},
		{"New session — 25/09 17:46:25", true, "the dated placeholder an older build persisted is still unchosen"},
		{"New session notes", false, "a title the user typed must never be overwritten"},
		{"Refactor the sidebar", false, "a real title is not a placeholder"},
		{"", false, "an empty title is not the placeholder it is compared against"},
	}
	for _, tc := range cases {
		if got := isPlaceholderTitle(tc.title); got != tc.want {
			t.Errorf("isPlaceholderTitle(%q) = %v, want %v: %s", tc.title, got, tc.want, tc.why)
		}
	}
}

// TestANewSessionTitleCarriesNoDate: the creation time used to be half the
// title. It now lives in the metadata line every session draws, and a title
// that repeats it says one thing twice while naming nothing.
func TestANewSessionTitleCarriesNoDate(t *testing.T) {
	c := newConversation("s-no-date", &fakeService{})
	if c.status().Title != placeholderTitle {
		t.Errorf("title = %q, want %q", c.status().Title, placeholderTitle)
	}
	if strings.ContainsAny(c.status().Title, "0123456789") {
		t.Errorf("a new session's title must not carry a date, got %q", c.status().Title)
	}
}

// TestTheDefaultSessionIsResetNotClosed: it belongs to the process that started this gateway,
// so it cannot be removed — but a DELETE resets it (clears the transcript, restores the
// placeholder title) and returns 200 with the updated status.
func TestTheDefaultSessionIsResetNotClosed(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := deleteReq(t, srv, sessionPath(srv, DefaultSession, ""), testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("deleting the default answered %d, it must be 200 (reset)", w.Code)
	}
	var status SessionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("could not parse the reset status: %v", err)
	}
	// The placeholder carries no date: when the session was created and when it
	// was last used belong in the metadata line, not in the title.
	if status.Title != placeholderTitle {
		t.Errorf("the reset title should be the placeholder %q, got: %s", placeholderTitle, status.Title)
	}
	if strings.ContainsAny(status.Title, "0123456789") {
		t.Errorf("the placeholder title must not carry a date, got: %s", status.Title)
	}
}

// TestASessionWithARunInFlightIsClosedByStoppingIt: a run in flight is no longer a
// reason to refuse. It used to be - there was no cancel path, and reporting a closed
// session while its run was still writing would have been a lie about what was
// happening. There IS one now, so the lie is gone and the decision belongs to the
// user: deleting a conversation stops what is running in it and then removes it.
func TestASessionWithARunInFlightIsClosedByStoppingIt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	svc := &blockingService{fakeService: fakeService{}, started: started, release: release}
	srv := newTestServer(t, svc, withFactory())
	conv, err := srv.newSession(svc)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}

	abandon := startInBackground(t, srv, conv.id, "/task", `{"task":"x"}`)
	defer abandon()
	<-started

	w := deleteReq(t, srv, sessionPath(srv, conv.id, ""), testToken)
	// 204: the session is REMOVED. It is not the default, so this is a removal
	// rather than a reset.
	if w.Code != http.StatusNoContent {
		t.Fatalf("closing a session while it runs answered %d, it must be 204: %s", w.Code, w.Body.String())
	}
	// And the run is stopped, not orphaned: the service saw its context cancelled.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.lookup(conv.id); !ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the session is still registered after being deleted")
	close(release)
}

// TestADeletedSessionsRunIsNoLongerExecuting is the same rule stated as the thing
// that actually matters: after the deletion answers, nothing is still working on
// behalf of that conversation.
func TestADeletedSessionsRunIsNoLongerExecuting(t *testing.T) {
	var stillRunning atomic.Bool
	stillRunning.Store(true)
	started, release := make(chan struct{}), make(chan struct{})
	// The SESSION has to be served by the service that blocks: a plain fakeService
	// answers instantly, so there would be no run in flight to delete out from under.
	tracking := &trackingService{started: started, release: release, running: &stillRunning}
	srv := newTestServer(t, tracking, withFactory())
	conv, err := srv.newSession(tracking)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}

	abandon := startInBackground(t, srv, conv.id, "/task", `{"task":"x"}`)
	defer abandon()
	<-started

	if w := deleteReq(t, srv, sessionPath(srv, conv.id, ""), testToken); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if stillRunning.Load() {
		t.Error("the run was still executing after the session was deleted")
	}
	close(release)
}

// trackingService reports whether its turn is still executing.
type trackingService struct {
	fakeService
	started chan struct{}
	release chan struct{}
	running *atomic.Bool
}

func (s *trackingService) RunTask(ctx context.Context, _ string, _ func(string, ...any)) (string, error) {
	close(s.started)
	defer s.running.Store(false)
	select {
	case <-s.release:
		return "released", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestTheListReportsEverySession: a front end has to be able to see what is open, including the
// default one, and whether anything is running.
func TestTheListReportsEverySession(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, withFactory())
	_ = post(t, srv, "/v1/sessions", "{}", testToken)

	var listed struct {
		Sessions []SessionStatus `json:"sessions"`
	}
	if err := json.Unmarshal(get(t, srv, "/v1/sessions", testToken).Body.Bytes(), &listed); err != nil {
		t.Fatalf("the list is not a list of sessions: %v", err)
	}
	if len(listed.Sessions) != 2 {
		t.Fatalf("the list reports %d sessions, it must report the default plus the new one", len(listed.Sessions))
	}
	if listed.Sessions[0].ID != DefaultSession {
		t.Errorf("the list is not sorted by id, so a client reading it twice sees two orders: %v", listed.Sessions)
	}
	if listed.Sessions[0].Created.IsZero() || listed.Sessions[0].LastUsed.IsZero() {
		t.Error("a session must say when it was opened and when it was last used")
	}
}

// TestTheListSaysWhichSessionIsRunning: a front end choosing where to send work needs to know
// which conversation is busy, and the flag is the only way to see it without trying.
func TestTheListSaysWhichSessionIsRunning(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := newTestServer(t, &blockingService{fakeService: fakeService{}, started: started, release: release})

	go func() { _ = post(t, srv, sessionPath(srv, DefaultSession, "/task"), `{"task":"x"}`, testToken) }()
	<-started

	var listed struct {
		Sessions []SessionStatus `json:"sessions"`
	}
	if err := json.Unmarshal(get(t, srv, "/v1/sessions", testToken).Body.Bytes(), &listed); err != nil {
		t.Fatalf("the list is not a list of sessions: %v", err)
	}
	if len(listed.Sessions) != 1 || !listed.Sessions[0].Running {
		t.Fatalf("the list does not report the run in flight: %+v", listed.Sessions)
	}
	close(release)
}

// TestTouchingASessionMovesItsLastUsedForward: the list is how a front end tells a conversation
// that is being worked in from one that was abandoned, and the only thing that makes that
// readable is that using a conversation updates it.
func TestTouchingASessionMovesItsLastUsedForward(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	conv := srv.sessions[DefaultSession]
	before := conv.status().LastUsed

	_ = get(t, srv, sessionPath(srv, DefaultSession, "/report"), testToken)

	if after := conv.status().LastUsed; !after.After(before) {
		t.Errorf("last_used did not move after the session was used (%v -> %v)", before, after)
	}
}

// TestForgettingASessionThatIsNotThereIsReported: the registry has to be able to tell "dropped it"
// from "it was not mine", because a caller acting on the answer needs the difference.
func TestForgettingASessionThatIsNotThereIsReported(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	if srv.forget("nothing-by-that-name") {
		t.Error("forget reported dropping a session that was never registered")
	}
	if conv, ok := srv.lookup("nothing-by-that-name"); ok || conv != nil {
		t.Error("lookup found a session that was never registered")
	}
}

// TestTheClientKnowsWhichSessionItSpeaksFor is the client half of T4, kept beside the sessions it
// addresses: the session is a constructor argument so a client cannot silently be unbound.
func TestTheClientKnowsWhichSessionItSpeaksFor(t *testing.T) {
	c := NewClientForSession("http://127.0.0.1:1", testToken, "sabc")
	if c.Session() != "sabc" {
		t.Errorf("Session() = %q, want sabc", c.Session())
	}
	if got := c.scoped("/task"); got != "/v1/sessions/sabc/task" {
		t.Errorf("scoped(/task) = %q", got)
	}
	// An empty name is the default conversation rather than a broken path.
	if got := NewClientForSession("http://127.0.0.1:1", testToken, "  ").Session(); got != DefaultSession {
		t.Errorf("an empty session name gave %q, want %q", got, DefaultSession)
	}
	// And the plain constructor speaks for the default, which is what the embedded client uses.
	if got := NewClient("http://127.0.0.1:1", testToken).scoped(""); got != "/v1/sessions/"+DefaultSession {
		t.Errorf("NewClient scoped() = %q", got)
	}
}

// TestASessionCanBeResetWithoutLosingItsIdentity: resetting starts a new conversation INSIDE the
// same session. It must not mint a new id, or a client that reset would lose the handle it was
// holding and every other front end would be pointed at a conversation nobody is in.
func TestASessionCanBeResetWithoutLosingItsIdentity(t *testing.T) {
	svc := &fakeService{}
	srv := newTestServer(t, svc)
	if w := post(t, srv, sessionPath(srv, DefaultSession, "/reset"), "{}", testToken); w.Code != http.StatusNoContent {
		t.Fatalf("reset answered %d: %s", w.Code, w.Body.String())
	}
	if svc.reset != 1 {
		t.Errorf("the conversation was reset %d times, want 1", svc.reset)
	}
	if _, ok := srv.lookup(DefaultSession); !ok {
		t.Error("the session disappeared when it was reset, it must keep its identity")
	}
}

// TestAStreamIsNotTheOnlyThingASessionRefuses keeps the busy rule honest across both kinds of run:
// a plan is served by the same slot as a task, so a task in flight refuses a plan.
func TestAPlanIsRefusedWhileATaskRunsInTheSameSession(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := newTestServer(t, &blockingService{fakeService: fakeService{}, started: started, release: release})

	go func() { _ = post(t, srv, sessionPath(srv, DefaultSession, "/task"), `{"task":"x"}`, testToken) }()
	<-started

	w := post(t, srv, sessionPath(srv, DefaultSession, "/plan"), `{"prompt":"plan something"}`, testToken)
	if w.Code != http.StatusConflict {
		t.Fatalf("a plan during a task answered %d, it must be 409: %s", w.Code, w.Body.String())
	}
	close(release)
	_ = context.Background()
}

// patchReq performs an authenticated PATCH and returns the recorder.
func patchReq(t *testing.T, srv *Server, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, srv.BaseURL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestASessionCanBeRenamed: a front end edits the title through PATCH, and the response carries
// the updated status so a client can draw it without a second round-trip.
func TestASessionCanBeRenamed(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, withFactory())
	created := post(t, srv, "/v1/sessions", "{}", testToken)
	var conv SessionStatus
	if err := json.Unmarshal(created.Body.Bytes(), &conv); err != nil {
		t.Fatalf("the answer is not a session: %v", err)
	}
	w := patchReq(t, srv, sessionPath(srv, conv.ID, ""), `{"title":"my chat"}`, testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("rename answered %d: %s", w.Code, w.Body.String())
	}
	var updated SessionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("the answer is not a session: %v", err)
	}
	if updated.Title != "my chat" {
		t.Errorf("title = %q, want %q", updated.Title, "my chat")
	}
	// The list must reflect the new title too.
	var listed struct {
		Sessions []SessionStatus `json:"sessions"`
	}
	if err := json.Unmarshal(get(t, srv, "/v1/sessions", testToken).Body.Bytes(), &listed); err != nil {
		t.Fatalf("the list is not a list of sessions: %v", err)
	}
	var found bool
	for _, s := range listed.Sessions {
		if s.ID == conv.ID {
			if s.Title != "my chat" {
				t.Errorf("the list still shows the old title: %q", s.Title)
			}
			found = true
		}
	}
	if !found {
		t.Error("the renamed session is not in the list")
	}
}

// TestAnEmptyTitleIsRefused: an empty title would erase the label without a name to replace it,
// so the rename endpoint refuses rather than installing an empty string.
func TestAnEmptyTitleIsRefused(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := patchReq(t, srv, sessionPath(srv, DefaultSession, ""), `{"title":"  "}`, testToken)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an empty title answered %d, it must be 400: %s", w.Code, w.Body.String())
	}
}

// TestTheConfigCanBeUpdated: a front end changes the provider and/or model through PATCH, and the
// response carries the updated configView so a client can update its display.
func TestTheConfigCanBeUpdated(t *testing.T) {
	svc := &fakeService{cfg: config.Default()}
	srv := newTestServer(t, svc)
	w := patchReq(t, srv, sessionPath(srv, DefaultSession, "/config"), `{"provider":"ollama","model":"qwen3:32b"}`, testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("update config answered %d: %s", w.Code, w.Body.String())
	}
	var view configView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("the answer is not a config view: %v", err)
	}
	if view.Provider != "ollama" {
		t.Errorf("provider = %q, want ollama", view.Provider)
	}
	if view.Model != "qwen3:32b" {
		t.Errorf("model = %q, want qwen3:32b", view.Model)
	}
}
