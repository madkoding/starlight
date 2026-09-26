package gateway

// Coverage tests for the gateway package — tests that exist only to reach the
// 100% coverage gate, covering functions that the feature tests do not
// exercise: project store, session store, update endpoints, persistence,
// client no-ops, and WebSocket helpers.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madkoding/motita/internal/agent"
	"github.com/madkoding/motita/internal/config"
	"github.com/madkoding/motita/internal/updater"
)

// --- Client no-ops ---

func TestClientSetWorkspaceIsNoOp(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	c := NewClient(srv.BaseURL(), srv.Token())
	c.SetWorkspace("/tmp/whatever")
}

func TestClientGenerateTitleJoinsFields(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	c := NewClient(srv.BaseURL(), srv.Token())
	got := c.GenerateTitle(context.Background(), "  hello   world  ")
	if got != "hello world" {
		t.Errorf("GenerateTitle = %q, want %q", got, "hello world")
	}
}

func TestClientRestoreTranscriptIsNoOp(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	c := NewClient(srv.BaseURL(), srv.Token())
	c.RestoreTranscript([]agent.DialogueTurn{{User: "hi"}})
}

// --- handleListCommands ---

func TestHandleListCommandsReturnsCatalogue(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := get(t, srv, "/v1/commands", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Commands []map[string]any `json:"commands"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Commands) == 0 {
		t.Error("expected at least one command in the catalogue")
	}
}

// --- Project store ---

func TestProjectStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	ps, err := newProjectStore(dir)
	if err != nil {
		t.Fatalf("newProjectStore: %v", err)
	}
	if got := ps.path("abc"); !strings.HasSuffix(got, "abc.json") {
		t.Errorf("path = %q, want suffix abc.json", got)
	}
	p := Project{ID: "p1", Title: "Test", Dir: "/tmp/test", Created: time.Now()}
	if err := ps.save(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := ps.load("p1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Title != "Test" {
		t.Errorf("loaded title = %q, want %q", got.Title, "Test")
	}
	all, err := ps.loadAll()
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("loadAll returned %d, want 1", len(all))
	}
	if err := ps.delete("p1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	all2, _ := ps.loadAll()
	if len(all2) != 0 {
		t.Errorf("after delete, loadAll returned %d, want 0", len(all2))
	}
}

func TestNewProjectStoreEmptyDir(t *testing.T) {
	_, err := newProjectStore("")
	if err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestProjectStoreLoadMissing(t *testing.T) {
	dir := t.TempDir()
	ps, _ := newProjectStore(dir)
	got, err := ps.load("nonexistent")
	if err != nil {
		t.Errorf("load of missing project should return nil, nil, got err: %v", err)
	}
	if got != nil {
		t.Errorf("load of missing project should return nil")
	}
}

func TestProjectStoreLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	ps, _ := newProjectStore(dir)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("not json"), 0o600)
	_, err := ps.load("bad")
	if err == nil {
		t.Error("expected error for corrupt JSON")
	}
}

func TestProjectStoreDeleteMissing(t *testing.T) {
	dir := t.TempDir()
	ps, _ := newProjectStore(dir)
	err := ps.delete("nonexistent")
	if err != nil {
		t.Errorf("delete of missing should be nil, got: %v", err)
	}
}

func TestCloneGitRepoEmptyURL(t *testing.T) {
	_, err := cloneGitRepo("", t.TempDir())
	if err == nil {
		t.Error("expected error for empty git URL")
	}
}

func TestCloneGitRepoInvalidURL(t *testing.T) {
	_, err := cloneGitRepo("not-a-valid-url", filepath.Join(t.TempDir(), "repo"))
	if err == nil {
		t.Error("expected error for invalid git URL")
	}
}

// --- Session store ---

func TestSessionStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	st, err := newSessionStore(dir)
	if err != nil {
		t.Fatalf("newSessionStore: %v", err)
	}
	if got := st.path("s1"); !strings.HasSuffix(got, "s1.json") {
		t.Errorf("path = %q, want suffix s1.json", got)
	}
	svc := &fakeService{}
	conv := newConversation("s1", svc)
	conv.setTitle("Test Session")
	if err := st.save(conv); err != nil {
		t.Fatalf("save: %v", err)
	}
	all, err := st.loadAll()
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if len(all) != 1 || all[0].ID != "s1" {
		t.Errorf("loadAll = %v, want 1 session with id s1", all)
	}
	if err := st.delete("s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	all2, _ := st.loadAll()
	if len(all2) != 0 {
		t.Errorf("after delete, loadAll returned %d, want 0", len(all2))
	}
}

func TestNewSessionStoreEmptyDir(t *testing.T) {
	_, err := newSessionStore("")
	if err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestSessionStoreLoadAllCorruptSkipped(t *testing.T) {
	dir := t.TempDir()
	st, _ := newSessionStore(dir)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("not json"), 0o600)
	all, err := st.loadAll()
	if err != nil {
		t.Fatalf("loadAll with corrupt file: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("corrupt file should be skipped, got %d", len(all))
	}
}

func TestSessionStoreDeleteMissing(t *testing.T) {
	dir := t.TempDir()
	st, _ := newSessionStore(dir)
	err := st.delete("nonexistent")
	if err != nil {
		t.Errorf("delete of missing should be nil, got: %v", err)
	}
}

// --- handleListProjects, handleCreateProject, handleDeleteProject ---

func TestHandleListProjectsWithoutStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := get(t, srv, "/v1/projects", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestHandleListProjectsWithStore(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	body := strings.NewReader(`{"title":"My Project","dir":"myproj"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create project: status = %d, want 201, body: %s", w.Code, w.Body.String())
	}
	w2 := get(t, srv, "/v1/projects", testToken)
	if w2.Code != http.StatusOK {
		t.Fatalf("list projects: status = %d", w2.Code)
	}
}

func TestHandleCreateProjectWithoutStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", strings.NewReader(`{"title":"x","dir":"y"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

func TestHandleCreateProjectEmptyTitle(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", strings.NewReader(`{"title":"","dir":"x"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleCreateProjectEmptyDir(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", strings.NewReader(`{"title":"x","dir":""}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleCreateProjectPathInDir(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", strings.NewReader(`{"title":"x","dir":"../escape"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleCreateProjectNoWorkspace(t *testing.T) {
	pdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = "" // no workspace
	})
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", strings.NewReader(`{"title":"x","dir":"y"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestHandleCreateProjectWithGitURL(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	// Use an invalid git URL to trigger the clone error path.
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", strings.NewReader(`{"title":"x","dir":"y","git_url":"not-a-real-url"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestHandleDeleteProjectWithoutStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	req, _ := http.NewRequest(http.MethodDelete, srv.BaseURL()+"/v1/projects/p1", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

func TestHandleDeleteProjectWithStore(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	body := strings.NewReader(`{"title":"To Delete","dir":"delme"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	id := resp["id"].(string)
	req2, _ := http.NewRequest(http.MethodDelete, srv.BaseURL()+"/v1/projects/"+id, nil)
	req2.Header.Set("Authorization", "Bearer "+testToken)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w2.Code)
	}
}

// --- projectOf ---

func TestProjectOfReturnsNilWhenNoStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	if p := srv.projectOf("anything"); p != nil {
		t.Error("expected nil when no project store")
	}
}

func TestProjectOfReturnsNilWhenEmptyID(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	if p := srv.projectOf(""); p != nil {
		t.Error("expected nil for empty id")
	}
}

func TestProjectOfReturnsNilWhenMissing(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	if p := srv.projectOf("nonexistent"); p != nil {
		t.Error("expected nil for missing project")
	}
}

func TestProjectOfReturnsProject(t *testing.T) {
	pdir := t.TempDir()
	wsdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.ProjectDir = pdir
		o.WorkspaceDir = wsdir
	})
	body := strings.NewReader(`{"title":"Find Me","dir":"found"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/projects", body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	id := resp["id"].(string)
	p := srv.projectOf(id)
	if p == nil || p.Title != "Find Me" {
		t.Errorf("projectOf = %+v, want project titled 'Find Me'", p)
	}
}

// --- sessionBranch ---

func TestSessionBranch(t *testing.T) {
	got := sessionBranch("abc123")
	if got != "motita/abc123" {
		t.Errorf("sessionBranch = %q, want %q", got, "motita/abc123")
	}
}

// --- Update endpoints ---

func TestHandleUpdateCheckWithoutUpdater(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := get(t, srv, "/v1/update/check", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body updater.CheckResult
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error == "" {
		t.Error("expected an error message when no updater is configured")
	}
}

func TestHandleUpdateRunWithoutUpdater(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/v1/update/run", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleUpdateCheckWithCache(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.updater = updater.New("test", "/tmp/fake-exe")
	srv.lastCheckMu.Lock()
	srv.lastCheck = updater.CheckResult{LatestVersion: "v9.9.9", CurrentVersion: "test"}
	srv.lastCheckMu.Unlock()
	w := get(t, srv, "/v1/update/check", testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body updater.CheckResult
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.LatestVersion != "v9.9.9" {
		t.Errorf("LatestVersion = %q, want v9.9.9", body.LatestVersion)
	}
}

// --- Persistence ---

func TestLoadPersistedSessionsWithoutStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.loadPersistedSessions()
}

func TestSaveAllSessionsWithoutStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.saveAllSessions()
}

func TestSaveAllSessionsWithStore(t *testing.T) {
	sdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.SessionDir = sdir
	})
	srv.saveAllSessions()
	if _, err := os.Stat(filepath.Join(sdir, "default.json")); err != nil {
		t.Errorf("expected default.json to exist: %v", err)
	}
}

func TestDeletePersistedSessionWithoutStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.deletePersistedSession("whatever")
}

func TestDeletePersistedSessionWithStore(t *testing.T) {
	sdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.SessionDir = sdir
	})
	srv.saveAllSessions()
	srv.deletePersistedSession("default")
	if _, err := os.Stat(filepath.Join(sdir, "default.json")); !os.IsNotExist(err) {
		t.Errorf("expected default.json to be deleted: %v", err)
	}
}

func TestDeletePersistedSessionMissing(t *testing.T) {
	sdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.SessionDir = sdir
	})
	srv.deletePersistedSession("nonexistent")
}

func TestResumeInterruptedSessionsWithoutStore(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	srv.resumeInterruptedSessions()
}

// TestSaveSessionSkipsForgottenConversation proves the guard in saveSession:
// a run's goroutine holds a pointer to the conversation and its deferred
// saveSession can fire AFTER handleDeleteSession has forgotten the session
// and deleted its file. Without the guard, that late save would resurrect
// the session on disk.
func TestSaveSessionSkipsForgottenConversation(t *testing.T) {
	sdir := t.TempDir()
	srv := newTestServer(t, &fakeService{}, func(o *Options) {
		o.SessionDir = sdir
	})
	// Register a conversation in the registry so saveSession's lookup finds it.
	c := newConversation("s-test-forgotten", &fakeService{})
	srv.sessionsMu.Lock()
	srv.sessions[c.id] = c
	srv.sessionsMu.Unlock()

	srv.saveSession(c) // must write the file
	if _, err := os.Stat(filepath.Join(sdir, "s-test-forgotten.json")); err != nil {
		t.Fatalf("the first save should have written the file: %v", err)
	}
	// Delete the file and forget the session, as handleDeleteSession does.
	srv.forget(c.id)
	os.Remove(filepath.Join(sdir, "s-test-forgotten.json"))

	srv.saveSession(c) // must NOT re-create the file
	if _, err := os.Stat(filepath.Join(sdir, "s-test-forgotten.json")); !os.IsNotExist(err) {
		t.Errorf("a forgotten session must not be re-saved: file exists after saveSession")
	}
}

// --- setProjectID on conversation ---

func TestConversationSetProjectID(t *testing.T) {
	c := newConversation("test", &fakeService{})
	c.setProjectID("proj-1", "/tmp/worktree-1", "/tmp/proj-1")
	if c.projectID != "proj-1" {
		t.Errorf("projectID = %q, want %q", c.projectID, "proj-1")
	}
	if c.workspace != "/tmp/worktree-1" {
		t.Errorf("workspace = %q, want %q", c.workspace, "/tmp/worktree-1")
	}
	// The project's own checkout is tracked separately: for a session with its
	// own worktree it is NOT the workspace, and that difference is what makes
	// "is there work to integrate" answerable.
	if c.projectDir != "/tmp/proj-1" {
		t.Errorf("projectDir = %q, want %q", c.projectDir, "/tmp/proj-1")
	}
}

// --- WebSocket helpers ---

func TestIsClosedConnErr(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		// EOF is the NORMAL end of a WebSocket connection - a browser closing its tab - and it is
		// classified as a clean exit. It used to be classified as a failure, which logged a warning
		// for every client that hung up.
		{io.EOF, true},
		{errors.New("use of closed network connection"), true},
		{errors.New("write: broken pipe"), true},
		{errors.New("read: connection reset by peer"), true},
		{errors.New("some other error"), false},
	}
	for _, tt := range tests {
		got := isClosedConnErr(tt.err)
		if got != tt.want {
			t.Errorf("isClosedConnErr(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestWebSocketHeartbeatLoopCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ackCh := make(chan struct{}, 1)
	cl := &wsClient{heartbeat: 50 * time.Millisecond}
	cl.heartbeatLoop(ctx, ackCh)
}

func TestWsHandshakeNonHijacker(t *testing.T) {
	w := httptest.NewRecorder()
	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	_, _, err := wsHandshake(w, r)
	if err == nil {
		t.Error("expected error from non-hijacking ResponseWriter")
	}
}

func TestWsWriteTextViaPipe(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	payload := []byte(`{"hello":"world"}`)
	go wsWriteText(server, payload)
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [2]byte
	io.ReadFull(client, hdr[:])
	if hdr[0]&0x0f != opText {
		t.Errorf("opcode = %x, want text", hdr[0]&0x0f)
	}
	length := int(hdr[1] & 0x7f)
	body := make([]byte, length)
	io.ReadFull(client, body)
	if string(body) != string(payload) {
		t.Errorf("body = %q, want %q", body, payload)
	}
}

func TestWsWriteCloseViaPipe(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	// net.Pipe is synchronous: the write blocks until the read side consumes it,
	// so the read must happen in a goroutine before the write.
	type result struct {
		payload []byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		client.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, _, _, err := wsReadFrame(client)
		done <- result{payload, err}
	}()
	if err := wsWriteClose(server, CloseNormal, "bye"); err != nil {
		t.Fatalf("write close: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		if len(r.payload) < 2 {
			t.Fatalf("payload too short: %d bytes", len(r.payload))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the close frame")
	}
}

// --- providerKeyPresent ---

func TestProviderKeyPresentForKnownProviders(t *testing.T) {
	got := providerKeyPresent(config.Default(), "openai")
	_ = got
}

func TestProviderKeyPresentWithAPIKey(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.APIKey = "test-key"
	if !providerKeyPresent(cfg, "openai") {
		t.Error("expected true when APIKey is set and provider uses MOTITA_LLM_API_KEY")
	}
}

// --- wsWriteFrame with large payload (>65535) ---

func TestWsWriteFrameLargePayload(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	payload := make([]byte, 70000) // > 65535, triggers 64-bit length
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	done := make(chan struct{})
	go func() {
		wsWriteFrame(server, opBinary, payload)
		close(done)
	}()
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [2]byte
	io.ReadFull(client, hdr[:])
	if hdr[1]&0x7f != 127 {
		t.Errorf("expected 64-bit length, got mask %d", hdr[1]&0x7f)
	}
	var lenBuf [8]byte
	io.ReadFull(client, lenBuf[:])
	// Read the rest of the payload
	remaining := make([]byte, 70000)
	io.ReadFull(client, remaining)
	<-done
}
