package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stuckService ignores cancellation entirely.
//
// It is what a run that cannot be stopped looks like, and it is the only way to
// reach the bounded wait: every honest service ends when it is cancelled, so the
// timeout branch would otherwise be dead code that nobody has ever run.
type stuckService struct {
	fakeService
	started chan struct{}
	release chan struct{}
}

func (s *stuckService) RunTask(ctx context.Context, _ string, _ func(string, ...any)) (string, error) {
	close(s.started)
	// Deliberately NOT selecting on ctx.Done(): this turn refuses to stop, which
	// is exactly the case the deletion has to survive.
	<-s.release
	return "finally released", nil
}

// newStuckServer returns a server whose service refuses to stop, with a session
// factory so a session of its own can be created and served by the same service.
//
// It also shrinks the deletion's wait: reaching the real ten-second timeout would
// make every test that uses it a ten-second test, and that is the reason the branch
// went unexercised to begin with.
func newStuckServer(t *testing.T) (*Server, *stuckService) {
	t.Helper()
	svc := &stuckService{started: make(chan struct{}), release: make(chan struct{})}
	srv := newTestServer(t, svc, func(o *Options) {
		o.SessionDir = t.TempDir()
		o.NewService = func() (Service, error) { return svc, nil }
	})
	// Released at the end, so the goroutine does not outlive the test - but only at
	// the end, so the run is still stuck while the deletion is refused.
	//
	// And then WAITED for. Releasing it is not enough: the run then finishes normally
	// and its goroutine still generates an auto-title and saves the session, a write
	// that would race t.TempDir's RemoveAll. waitForNoRun makes the temp dirs quiet
	// before they are removed - isRunning is false only after the goroutine's last
	// defer.
	t.Cleanup(func() {
		close(svc.release)
		waitForNoRun(t, srv)
	})
	shrinkDeleteStopTimeout(t)
	return srv, svc
}

// shrinkDeleteStopTimeout makes the deletion's wait short for one test and restores
// it afterwards.
func shrinkDeleteStopTimeout(t *testing.T) {
	t.Helper()
	prev := deleteStopTimeout
	deleteStopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { deleteStopTimeout = prev })
}

// TestStopRunForDeletionReportsNothingToStop: stopping a conversation with no run is
// not an error and not a wait - it reports that there was nothing to stop, and
// settled is true so the caller can go on and delete. This is the common case: most
// deletions are of quiet sessions.
func TestStopRunForDeletionReportsNothingToStop(t *testing.T) {
	c := newConversation("quiet", &fakeService{})
	stopped, settled := c.stopRunForDeletion(time.Second)
	if stopped {
		t.Error("a conversation with no run reported that it stopped one")
	}
	if !settled {
		t.Error("a conversation with no run reported that it had not settled")
	}
}

// TestStopRunForDeletionStopsAnHonestRun: the ordinary case, and the one the whole
// feature is for - a run that is in flight and behaving is stopped, and the wait
// returns as soon as it has settled.
func TestStopRunForDeletionStopsAnHonestRun(t *testing.T) {
	var sawCancel atomic.Bool
	srv := newTestServer(t, &fakeService{task: func(ctx context.Context, _ string, _ func(string, ...any)) (string, error) {
		<-ctx.Done()
		sawCancel.Store(true)
		return "", ctx.Err()
	}})
	conv, _ := srv.lookup(DefaultSession)

	abandon := startInBackground(t, srv, DefaultSession, "/task", `{"task":"work"}`)
	defer abandon()
	waitForRunning(t, srv, DefaultSession)

	stopped, settled := conv.stopRunForDeletion(5 * time.Second)
	if !stopped || !settled {
		t.Fatalf("an honest run was reported as stopped=%v settled=%v, want true/true", stopped, settled)
	}
	if !sawCancel.Load() {
		t.Error("the run was reported as stopped but never saw its cancellation")
	}
}

// TestStopRunForDeletionTimesOutOnAStuckRun: the wait is BOUNDED.
//
// A run that ignores its cancellation must not hold the request open forever, and it
// must not let the session be deleted behind a turn that is still writing. It is
// reported as stopped-on-paper (stopped, not settled) and the handler turns that into
// a refusal that says so - the user is told the truth instead of being left waiting.
func TestStopRunForDeletionTimesOutOnAStuckRun(t *testing.T) {
	srv, svc := newStuckServer(t)
	conv, ok := srv.lookup(DefaultSession)
	if !ok {
		t.Fatal("the default conversation is missing")
	}

	abandon := startInBackground(t, srv, DefaultSession, "/task", `{"task":"refuse to stop"}`)
	defer abandon()
	<-svc.started
	// Released only when the test is done: the run has to still be stuck while the
	// wait below times out, or it settles and the timeout is never reached. The
	// cleanup is registered by newStuckServer for exactly that.

	stopped, settled := conv.stopRunForDeletion(30 * time.Millisecond)
	if !stopped {
		t.Error("there was a run in flight and it was not reported")
	}
	if settled {
		t.Error("a run that ignores its cancellation was reported as settled")
	}
}

// TestDeletingASessionWhoseRunWillNotStopIsRefused: the handler's half of the same
// rule. The session must NOT be removed, and the refusal must say why.
func TestDeletingASessionWhoseRunWillNotStopIsRefused(t *testing.T) {
	srv, svc := newStuckServer(t) // also shrinks the wait
	created := newSessionFor(t, srv)

	abandon := startInBackground(t, srv, created.ID, "/task", `{"task":"refuse to stop"}`)
	defer abandon()
	<-svc.started

	del := send(t, srv, http.MethodDelete, "/v1/sessions/"+created.ID, testToken, "")
	if del.Code != http.StatusConflict {
		t.Fatalf("deleting a session whose run will not stop answered %d, want 409: %s", del.Code, del.Body.String())
	}
	if !strings.Contains(del.Body.String(), "did not stop in time") {
		t.Errorf("the refusal must say the run did not stop: %s", del.Body.String())
	}
	// The session is still there - that is the point of refusing.
	if _, ok := srv.lookup(created.ID); !ok {
		t.Error("the session was deleted even though its run had not stopped")
	}
}

// TestDeletingAProjectWhoseRunWillNotStopIsRefused: the project handler's half. The
// project must survive, and it must still be listed.
func TestDeletingAProjectWhoseRunWillNotStopIsRefused(t *testing.T) {
	svc := &stuckService{started: make(chan struct{}), release: make(chan struct{})}
	srv := newTestServer(t, svc, func(o *Options) {
		o.SessionDir = t.TempDir()
		o.ProjectDir = t.TempDir()
		o.WorkspaceDir = t.TempDir()
		o.NewService = func() (Service, error) { return svc, nil }
	})
	t.Cleanup(func() {
		close(svc.release)
		waitForNoRun(t, srv)
	})
	shrinkDeleteStopTimeout(t)

	proj := post(t, srv, "/v1/projects", `{"title":"p","dir":"projdir"}`, testToken)
	if proj.Code != http.StatusCreated {
		t.Fatalf("create project: %d %s", proj.Code, proj.Body.String())
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(proj.Body.Bytes(), &p); err != nil {
		t.Fatalf("decoding the project: %v", err)
	}

	sw := post(t, srv, "/v1/sessions", `{"project_id":"`+p.ID+`"}`, testToken)
	if sw.Code != http.StatusCreated {
		t.Fatalf("create session in project: %d %s", sw.Code, sw.Body.String())
	}
	var created SessionStatus
	if err := json.Unmarshal(sw.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding the created session: %v", err)
	}

	abandon := startInBackground(t, srv, created.ID, "/task", `{"task":"refuse to stop"}`)
	defer abandon()
	<-svc.started

	del := send(t, srv, http.MethodDelete, "/v1/projects/"+p.ID, testToken, "")
	if del.Code != http.StatusConflict {
		t.Fatalf("deleting a project whose run will not stop answered %d, want 409: %s", del.Code, del.Body.String())
	}
	if !strings.Contains(del.Body.String(), "did not stop in time") {
		t.Errorf("the refusal must say the run did not stop: %s", del.Body.String())
	}
	// The project is still there.
	if w := get(t, srv, "/v1/projects", testToken); !strings.Contains(w.Body.String(), p.ID) {
		t.Errorf("the project was removed even though its run had not stopped: %s", w.Body.String())
	}
}
