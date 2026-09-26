package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// deleteProbeService is a service whose turn runs until its context is cancelled, and
// which records that it SAW the cancellation.
//
// The recording is the point. A deletion that forgot to cancel would still return
// 204, because the handler forgets the conversation straight afterwards - the
// session would be gone from the list while its turn kept running, which is
// exactly the state this file exists to prevent. `sawCancel` is what tells the two
// apart: the run must have been asked to stop, not merely abandoned.
type deleteProbeService struct {
	fakeService
	sawCancel atomic.Bool
	// started is closed once the turn is in flight, so a test deletes only after
	// there really is a run to stop.
	started chan struct{}
	// release ends the turn without cancelling it, so a test that does not delete
	// can still let its goroutine finish.
	release chan struct{}
}

func (b *deleteProbeService) RunTask(ctx context.Context, _ string, _ func(string, ...any)) (string, error) {
	close(b.started)
	select {
	case <-b.release:
		return "finished on its own", nil
	case <-ctx.Done():
		b.sawCancel.Store(true)
		return "", ctx.Err()
	}
}

// newDeleteProbeService returns a service that blocks, with its release channel
// closed at the end of the test so no goroutine outlives it.
func newDeleteProbeService(t *testing.T) *deleteProbeService {
	t.Helper()
	svc := &deleteProbeService{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(svc.release) })
	return svc
}

// newSessionFor creates a session of its own and returns its status. A session of
// its own matters: deleting "default" RESETS it rather than removing it, so a test
// about removal has to use a real one.
func newSessionFor(t *testing.T, srv *Server) SessionStatus {
	t.Helper()
	w := post(t, srv, "/v1/sessions", "", testToken)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", w.Code, w.Body.String())
	}
	var created SessionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding the created session: %v", err)
	}
	if created.ID == "" {
		t.Fatal("the created session has no id")
	}
	return created
}

// waitForRunStart waits until the service says its turn is in flight.
func waitForRunStart(t *testing.T, svc *deleteProbeService) {
	t.Helper()
	select {
	case <-svc.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never started")
	}
}

// TestDeletingASessionStopsItsRun is the whole point: the agent must stop working
// before the session it was working in is removed.
func TestDeletingASessionStopsItsRun(t *testing.T) {
	svc := newDeleteProbeService(t)
	srv := newTestServer(t, svc, func(o *Options) {
		o.SessionDir = t.TempDir()
		// The new session has to be served by the SAME blocking service: a fresh
		// fakeService would answer instantly, and there would be no run in flight
		// to stop.
		o.NewService = func() (Service, error) { return svc, nil }
	})
	created := newSessionFor(t, srv)

	abandon := startInBackground(t, srv, created.ID, "/task", `{"task":"work forever"}`)
	defer abandon()
	waitForRunStart(t, svc)

	// The deletion must SUCCEED - that is the change. It used to be refused with a
	// 409 telling the user to come back when the run had finished, which made the
	// decision conditional on a turn they had no way to end from here.
	del := send(t, srv, http.MethodDelete, "/v1/sessions/"+created.ID, testToken, "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete while running: status = %d, want 204 (body %s)", del.Code, del.Body.String())
	}

	// And the run must have been STOPPED, not left behind: the service has to have
	// seen its context cancelled by the time the deletion answered.
	if !svc.sawCancel.Load() {
		t.Error("the deletion removed the session without stopping its run: the turn is still executing")
	}

	// Nothing of it survives: not in the registry, not on disk.
	if _, ok := srv.lookup(created.ID); ok {
		t.Error("the session is still registered after being deleted")
	}
	if _, err := os.Stat(filepath.Join(srv.opts.SessionDir, created.ID+".json")); !os.IsNotExist(err) {
		t.Errorf("the session file survived the deletion (stat err = %v)", err)
	}
}

// TestDeletingASessionWaitsForTheRunToUnwind: the stop has to be COMPLETE before the
// session is removed.
//
// A cancelled run still unwinds afterwards - it appends its final events, generates
// an auto-title and saves the session - and that last save is a write to a
// conversation the deletion is in the middle of removing. The saveSession guard
// stops that write from resurrecting the file; the WAIT is what stops it happening
// at all. By the time DELETE answers, no goroutine is still working on the turn.
func TestDeletingASessionWaitsForTheRunToUnwind(t *testing.T) {
	// A service that observes cancellation and then keeps working briefly, which is
	// the shape a real unwind has: the turn stops, and there is still work left.
	var finished atomic.Bool
	unw := &unwindingService{finished: &finished}
	srv := newTestServer(t, unw, func(o *Options) {
		o.SessionDir = t.TempDir()
		o.NewService = func() (Service, error) { return unw, nil }
	})
	created := newSessionFor(t, srv)

	abandon := startInBackground(t, srv, created.ID, "/task", `{"task":"unwind slowly"}`)
	defer abandon()
	waitForRunning(t, srv, created.ID)

	del := send(t, srv, http.MethodDelete, "/v1/sessions/"+created.ID, testToken, "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204 (body %s)", del.Code, del.Body.String())
	}
	if !finished.Load() {
		t.Error("the deletion answered before the run had finished unwinding: the turn can still write to the session it removed")
	}
}

// unwindingService stops when cancelled, but not instantly: it does observable work
// after observing the cancellation, which is exactly what a deletion has to wait for.
type unwindingService struct {
	fakeService
	finished *atomic.Bool
}

func (u *unwindingService) RunTask(ctx context.Context, _ string, _ func(string, ...any)) (string, error) {
	<-ctx.Done()
	time.Sleep(150 * time.Millisecond)
	u.finished.Store(true)
	return "", ctx.Err()
}

// TestDeletingAProjectStopsItsSessionsRuns: removing a project removes the panel
// those sessions were reachable from, so a turn left running in one of them is work
// nobody can see or stop any more.
func TestDeletingAProjectStopsItsSessionsRuns(t *testing.T) {
	svc := newDeleteProbeService(t)
	srv := newTestServer(t, svc, func(o *Options) {
		o.SessionDir = t.TempDir()
		o.ProjectDir = t.TempDir()
		// A project's folder is created inside the workspace, so the gateway needs
		// one to be configured before it can accept a project at all.
		o.WorkspaceDir = t.TempDir()
		o.NewService = func() (Service, error) { return svc, nil }
	})

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

	abandon := startInBackground(t, srv, created.ID, "/task", `{"task":"work forever"}`)
	defer abandon()
	waitForRunStart(t, svc)

	del := send(t, srv, http.MethodDelete, "/v1/projects/"+p.ID, testToken, "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete project: status = %d, want 204 (body %s)", del.Code, del.Body.String())
	}
	if !svc.sawCancel.Load() {
		t.Error("the project was removed while a turn in one of its sessions kept running")
	}
}
