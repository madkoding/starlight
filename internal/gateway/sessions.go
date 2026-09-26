package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/madkoding/motita/internal/gitx"
)

// deleteStopTimeout is how long a deletion waits for a cancelled run to finish
// unwinding before giving up and reporting it.
//
// It is a var rather than a const so a test can set it small: reaching the timeout
// with the real value would mean a ten-second test, and the branch would go
// unexercised. The repository already uses this shape for its other seams
// (launchCommand, listenAndServe).
//
// Generous on purpose: the wait is normally milliseconds, because the cancellation
// kills the sandbox's whole process group and there is no command left to wait for.
// Ten seconds is long enough that a slow-but-honest unwind is never mistaken for a
// stuck one, and short enough that a user pressing Delete is not left staring at a
// request that never returns.
var deleteStopTimeout = 10 * time.Second

// DefaultSession is the conversation every gateway has, and the one an embedded client uses.
//
// It exists because the common case is one conversation, not zero: the terminal that started this
// process talks to this id, and a client that only ever wanted to talk to "the agent" should not
// have to open a conversation first in order to do it.
const DefaultSession = "default"

// defaultMaxSessions bounds how many conversations one process will hold.
//
// A conversation is not free: it keeps a transcript, a session and a reward attribution alive for
// as long as it exists. The ceiling is what stops a client that forgets to close what it opened
// from turning the agent into a memory leak. Eight is far more than the handful of front ends
// this serves and far less than anything that would matter on a machine that already runs
// commands.
const defaultMaxSessions = 8

// conversation is ONE agent conversation: the service that speaks for it, the right to run in
// it, and the question waiting to be answered in it.
//
// The run slot and the approval slot live HERE rather than on the Server, and that move is the
// whole feature. They used to live on the Server, with a comment saying why: an agent has one
// conversation, so two runs at once would interleave two tasks into one transcript. That reason
// is about a CONVERSATION and not about a process - so with several conversations there are
// several slots, and two front ends can work at the same time without sharing a transcript.
type conversation struct {
	id  string
	svc Service

	// stateMu guards the three small facts the list endpoint reads and the run slot. One lock
	// for all of them because they are always read together, and a lock per field would be
	// three chances to forget one.
	stateMu  sync.Mutex
	created  time.Time
	lastUsed time.Time
	running  bool
	// lastTask is the most recent task or plan prompt submitted to this
	// conversation. It is saved so that a session interrupted by a gateway
	// restart (an upgrade) can be resumed automatically: the new process
	// reads it from the persisted record and re-submits it.
	lastTask string
	// lastKind is "task" or "plan", recording which mode the last run was
	// in. An interrupted plan and an interrupted task resume differently.
	lastKind string
	// title is the human-readable label a front end draws for this conversation. It is empty
	// until the first turn completes and an auto-title is derived from it, and it may be
	// changed by the user at any time through the rename endpoint.
	title string
	// projectID is the project this conversation belongs to, or empty when it
	// is a free-standing session. A session that belongs to a project runs
	// with its workspace set to the project's directory.
	projectID string
	// workspace is the directory the agent works in. It is empty for a
	// free-standing session. For a session in a git project it is the
	// session's OWN worktree, and projectDir below is the project it branches
	// from; for one that belongs to a non-git project the two are the same.
	// It is read to report the directory and the branch a session is on, so a
	// front end can show them without another round-trip.
	workspace string
	// projectDir is the project's own checkout, and it is what a session's
	// branch is compared against when deciding whether there is work to
	// integrate. Empty for a free-standing session.
	//
	// It is tracked separately from workspace because they differ exactly when
	// the feature is working: a session with its own worktree reports that
	// worktree as its workspace, and comparing its branch against ITSELF would
	// always report "nothing to merge".
	projectDir string

	// current is the run in flight, and nil when there is none.
	//
	// It replaces the loose context.CancelFunc the run used to be: a run is now an object that
	// owns its events, its subscribers and its pending approval, and the conversation only has
	// to know WHICH one is live so a client that reconnects can find it. Guarded by stateMu,
	// the same lock as `running`, because the two are read together: the run slot being taken
	// IS a run being current.
	current *run
	// pending is the approval waiting to be answered, and nil when nothing is being asked.
	//
	// It lives on the CONVERSATION rather than inside the run so that a client which reconnects
	// can be told about it: the question was asked while it was away, and a client that is not
	// told will sit forever watching a run that is waiting for the answer it will never give.
	pending *pendingApproval
}

// pendingApproval is one question waiting for a human answer.
type pendingApproval struct {
	id      string
	ch      chan bool
	command string
	reason  string
	rule    string
}

func newConversation(id string, svc Service) *conversation {
	now := time.Now()
	return &conversation{
		id:       id,
		svc:      svc,
		created:  now,
		lastUsed: now,
		title:    placeholderTitle,
	}
}

// placeholderTitle is the title a session carries until something better names
// it.
//
// It used to embed the creation time ("New session — 02/01 15:04:05"), and the
// time is not gone - it moved to the session's own metadata line, where every
// session shows when it was last used. Two places showing one timestamp meant
// the title was half date, and a title is for the name.
const placeholderTitle = "New session"

// legacyPlaceholderPrefix is the opening of the dated placeholder older builds
// wrote. Sessions persisted by one of those builds are still on disk and still
// unnamed, so they must still be recognised as placeholders - otherwise a
// session created before this change would keep its date forever instead of
// being auto-titled on its next turn.
const legacyPlaceholderPrefix = "New session — "

// isPlaceholderTitle reports whether a title is one nobody chose: the current
// placeholder, or the dated one an older build wrote.
//
// An exact match is required for the current placeholder. A prefix match would
// also swallow a title the user set themselves ("New session notes"), and
// overwriting a name the user typed is worse than leaving a session unnamed.
func isPlaceholderTitle(title string) bool {
	return title == placeholderTitle || strings.HasPrefix(title, legacyPlaceholderPrefix)
}

// touch records that this conversation was just spoken to.
func (c *conversation) touch() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.lastUsed = time.Now()
}

// takeRunSlot reserves this conversation's one run slot, or reports that it is taken.
func (c *conversation) takeRunSlot() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.running {
		return false
	}
	c.running = true
	return true
}

// releaseRunSlot frees it. Called from a defer, so a failing or panicking run cannot leak the
// slot and leave this conversation refusing everybody forever.
func (c *conversation) releaseRunSlot() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.running = false
}

// isRunning reports whether a run is in flight here.
func (c *conversation) isRunning() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.running
}

// currentRun returns the run in flight, if there is one.
//
// This is what a reconnecting client is answered with, and it is why the client keeps nothing: it
// asks the gateway which run is live instead of remembering one.
func (c *conversation) currentRun() (*run, bool) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.current == nil {
		return nil, false
	}
	return c.current, true
}

// setCurrentRun installs a run as the live one.
//
// The slot is taken by the caller first (takeRunSlot), so by the time this runs there is no other
// run to displace.
func (c *conversation) setCurrentRun(r *run) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.current = r
}

// clearCurrentRun drops the live run, and is called from a defer so a failing or panicking run
// cannot leave a finished one looking live.
func (c *conversation) clearCurrentRun() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.current = nil
}

// setPendingApproval records the question being asked.
func (c *conversation) setPendingApproval(p *pendingApproval) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.pending = p
}

// pendingApprovalNow returns the question being asked, if any.
func (c *conversation) pendingApprovalNow() *pendingApproval {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.pending
}

// clearPendingApproval drops the question, and is called from a defer so a late answer cannot land
// on the next one.
func (c *conversation) clearPendingApproval() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.pending = nil
}

// cancelRun stops the run in flight and reports whether there was one to stop.
func (c *conversation) cancelRun() bool {
	rn, ok := c.currentRun()
	if !ok {
		return false
	}
	rn.cancel()
	return true
}

// stopRunForDeletion stops the run in flight and waits for the goroutine to
// finish, so that by the time this returns nothing is still executing on behalf
// of this conversation and nothing will write to it again.
//
// A plain cancellation is not enough for a DELETION. Cancelling only asks the run
// to stop: the goroutine then unwinds from wherever it was - killing the sandbox
// process group, appending its final events, running maybeAutoTitle, and reaching
// saveSession - and deleting underneath that leaves a conversation being torn
// down while a turn is still writing to it. Waiting for the run's own done channel
// is what makes "stop it and then delete it" true rather than two races that
// usually happen to be ordered.
//
// It reports whether there was a run to stop. The wait is bounded: a run that
// ignores its cancellation for longer than this is reported as stopped-on-paper
// rather than blocking the request forever, and the caller decides what to say.
func (c *conversation) stopRunForDeletion(timeout time.Duration) (bool, bool) {
	rn, ok := c.currentRun()
	if !ok {
		return false, true
	}
	rn.cancel()
	select {
	case <-rn.done:
		return true, true
	case <-time.After(timeout):
		return true, false
	}
}

// SessionStatus is what the list and create endpoints report about one conversation.
type SessionStatus struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	ProjectID string    `json:"project_id,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Created   time.Time `json:"created"`
	LastUsed  time.Time `json:"last_used"`
	Running   bool      `json:"running"`
	// Mergeable reports whether the session has work that can be integrated
	// back into the project's base branch. It is true when the session belongs
	// to a project, the session's branch (motita/<id>) exists, and it has
	// commits the base branch does not. A session that was never run in a
	// worktree, or whose work has already been merged, is not mergeable — and
	// the front end shows that as a disabled Integrate button rather than an
	// absent one, so the user knows the action exists even when it has nothing
	// to do yet.
	Mergeable bool `json:"mergeable,omitempty"`
	// Workspace is the directory this session actually runs in. For a session
	// in a git project that is its OWN worktree, not the project's checkout,
	// which is what lets two sessions work at once without editing each
	// other's files. It is empty for a free-standing session.
	Workspace string `json:"workspace,omitempty"`
	// Changes is how many uncommitted changes this session has in its own
	// working tree: modified, staged, deleted, renamed and untracked files,
	// each counted once. It is what the sidebar draws as a count, so a session
	// that has written something does not read as idle. Absent (0) for a
	// session with no workspace, or one that is not a repository.
	Changes int `json:"changes,omitempty"`
	// Worktree is the NAME of the session's worktree directory, which is its
	// id - the last path element rather than the whole path, because the path
	// is long, mostly identical between sessions, and would be truncated to
	// nothing useful in a narrow sidebar. It is what tells two sessions of one
	// project apart at a glance. Empty for a session without its own worktree.
	Worktree string `json:"worktree,omitempty"`
}

func (c *conversation) status() SessionStatus {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	st := SessionStatus{ID: c.id, Title: c.title, ProjectID: c.projectID, Created: c.created, LastUsed: c.lastUsed, Running: c.running}
	st.Workspace = c.workspace
	if c.workspace != "" {
		ctx := context.Background()
		st.Branch = gitx.Display(ctx, c.workspace)
		// The worktree is a session's OWN directory, and naming it here is what
		// tells two sessions of one project apart: they share a project, a
		// branch prefix and a title format, and the only thing that differs is
		// the directory each one runs in.
		//
		// Asked of git rather than compared with the project directory: a
		// session that fell back to the project's checkout has no worktree of
		// its own, and calling that directory one would be inventing a
		// distinction the filesystem does not make. The listing also settles
		// the symlinked and relative spellings of the same path, which a string
		// comparison does not.
		//
		// The project's own checkout IS a listed worktree of its repository, so
		// "git knows this path" is not the question - the question is whether
		// this path is somewhere OTHER than the project. Without that second
		// test a session that fell back to the project's checkout reports the
		// project's directory as its worktree, which is the one thing it is not.
		if c.projectDir != "" && !gitx.SamePath(c.workspace, c.projectDir) {
			if _, ok, err := gitx.LiveWorktreeAt(ctx, c.projectDir, c.workspace); err == nil && ok {
				st.Worktree = filepath.Base(c.workspace)
			}
		}
		// Changes is read from the session's own tree, so the count is the work
		// this session has done. An unreadable tree reports 0 rather than an
		// error: the count decorates a badge, and a badge that cannot be
		// computed should be absent rather than break the sidebar.
		if n, err := gitx.WorkingTreeChanges(ctx, c.workspace); err == nil {
			st.Changes = n
		}
		// Mergeable compares the session's branch against the PROJECT's branch,
		// not against whatever the session's own checkout is on. A session with
		// its own worktree reports its own branch (motita/<id>), so comparing
		// the two would always say "nothing ahead" - the one answer that is
		// never useful here.
		base := st.Branch
		if c.projectDir != "" {
			base = gitx.Display(ctx, c.projectDir)
		}
		// Mergeable: the session's branch exists and has commits the base
		// branch does not. A session that never ran in a worktree has no
		// branch, and one whose work was already merged has none ahead.
		branch := sessionBranch(c.id)
		if gitx.BranchExists(ctx, c.workspace, branch) {
			if ahead, _, err := gitx.CommitsBetween(ctx, c.workspace, base, branch); err == nil && len(ahead) > 0 {
				st.Mergeable = true
			}
		}
	}
	return st
}

// sessionWorktree gives a session its own checkout of a project, so two
// sessions in one project can work at the same time without editing each
// other's files.
//
// It returns the directory the session should run in. When a worktree cannot be
// made it returns the PROJECT's directory and the reason, because worktrees
// need git: a project folder that is not a repository, or one with no commits
// yet, must keep working exactly as it did before rather than failing to start
// a session. Turning "this project is not a repo" into "this session cannot
// start" would be a far worse answer than running in the directory the user
// actually pointed at.
//
// It is IDEMPOTENT, and that is not a convenience: the common case is a session
// whose worktree already exists - one restored after a gateway restart, or one
// the caller asked about twice - and `git worktree add` REFUSES a directory it
// has already registered. Measured: it fails with "Preparing worktree (checking
// out 'motita/<id>')" and a non-zero status, which would turn every restored
// session into one that could not run.
//
// The session's branch is the unit of isolation, and it OUTLIVES the worktree:
// a session whose worktree is removed and recreated finds its own commits again
// rather than starting over. That is why AddWorktree attaches an existing
// branch instead of insisting on a new one.
//
// The path is derived from the workspace root and the session id alone, so the
// same session always resolves to the same directory. This is the ONLY place
// that decides where a session runs: a second copy of that rule is how a
// session ends up reporting one directory while running in another.
func (s *Server) sessionWorktree(ctx context.Context, repoDir, sessionID string) (string, error) {
	if strings.TrimSpace(s.opts.WorkspaceDir) == "" {
		// Without a workspace root there is nowhere to put a worktree, and a
		// relative path would be resolved against the process's own directory.
		// The project directory is a correct answer and needs no extra state.
		return repoDir, nil
	}
	path := filepath.Join(s.opts.WorkspaceDir, "worktrees", sessionID)
	branch := sessionBranch(sessionID)
	// Already ours: a LIVE worktree of this repository at this path is the
	// worktree asked for, whatever created it.
	//
	// The test is the PATH, deliberately, and not the branch. A session whose
	// worktree the user moved to a feature branch is still working in its own
	// tree - and asking about the branch instead answers "not ours" for it,
	// which then sends this function down the add below. Measured: that add
	// fails ("already exists", non-zero), the error propagates as the fallback
	// to the PROJECT's directory, and two sessions end up editing one checkout -
	// the exact collision worktrees exist to prevent.
	if _, ok, err := gitx.LiveWorktreeAt(ctx, repoDir, path); err == nil && ok {
		return path, nil
	}
	if err := gitx.AddWorktree(ctx, repoDir, path, branch); err != nil {
		return repoDir, err
	}
	return path, nil
}

// setProjectID records which project this conversation belongs to, the project's
// own checkout, and the workspace the session actually runs in. The last two
// differ for a session with its own worktree, and both are needed: the workspace
// is where the agent writes, and the project directory is what its branch gets
// compared against.
func (c *conversation) setProjectID(pid, workspace, projectDir string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.projectID = pid
	c.workspace = workspace
	c.projectDir = projectDir
}

// setTitle sets the human-readable label for this conversation. Called after the first turn
// completes to derive an auto-title, and by the rename endpoint when the user edits one.
func (c *conversation) setTitle(t string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.title = t
}

// conversationKeyType is the context key the resolved conversation travels under. It is an
// unexported type so that nothing outside this package can collide with it.
type conversationKeyType struct{}

var conversationKey conversationKeyType

// convOf returns the conversation a request is about.
//
// It PANICS rather than falling back, and that is deliberate: a handler reached without a
// conversation is a programming error, and a fallback would answer about the DEFAULT conversation
// - which is the one failure this design exists to prevent. A loud panic in a test is worth more
// than a quiet answer about the wrong transcript.
//
// It can only be reached with a value because every scoped route goes through withConversation.
func convOf(r *http.Request) *conversation {
	c, ok := r.Context().Value(conversationKey).(*conversation)
	if !ok {
		panic("a gateway handler was reached without a conversation: it is not registered through withConversation")
	}
	return c
}

// withConversation resolves the {id} in the path to a live conversation and puts it in the
// request context, so that no handler has to parse a path.
//
// A request for a conversation that does not exist is answered HERE and never reaches the
// handler: a check each handler made for itself is a check one of them would forget, and the one
// that forgot would answer about the wrong conversation.
func (s *Server) withConversation(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		conv, ok := s.lookup(id)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("there is no session %q", id))
			return
		}
		conv.touch()
		h(w, r.WithContext(context.WithValue(r.Context(), conversationKey, conv)))
	})
}

// lookup finds a live conversation by id.
//
// The registry lock is held for the lookup and released before any work happens, so a long run in
// one conversation never blocks a client asking for another one.
func (s *Server) lookup(id string) (*conversation, bool) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	c, ok := s.sessions[id]
	return c, ok
}

// snapshot copies the registry, so the list can be built without holding the lock every request
// needs in order to find its conversation.
func (s *Server) snapshot() []*conversation {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	out := make([]*conversation, 0, len(s.sessions))
	for _, c := range s.sessions {
		out = append(out, c)
	}
	return out
}

// forget drops a conversation, and reports whether it was there.
func (s *Server) forget(id string) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false
	}
	delete(s.sessions, id)
	return true
}

// maxSessions is the ceiling in force, defaulted when none was configured.
func (s *Server) maxSessions() int {
	if s.opts.MaxSessions > 0 {
		return s.opts.MaxSessions
	}
	return defaultMaxSessions
}

// ErrCeilingReached is returned when the process is already holding as many conversations as it
// will. It is a SENTINEL rather than a message, because the status the caller answers with depends
// on which refusal this is: a gateway that cannot build conversations at all is a different thing
// from one that is full, and answering both with the same code would tell a client to give up when
// closing one session would have fixed it.
var ErrCeilingReached = errors.New("this gateway is at its ceiling of conversations")

// newSession mints one more conversation, or reports why it cannot.
//
// NewService is called while the registry lock is held. That is deliberate: the alternative is a
// capacity check that another request can invalidate between the check and the insert, and a
// factory that called back into the gateway would deadlock instead. Neither is worth the two
// lines it saves, so the contract is stated rather than worked around - NewService must not call
// back into the gateway.
func (s *Server) newSession(svc Service) (*conversation, error) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if len(s.sessions) >= s.maxSessions() {
		return nil, fmt.Errorf("%w: it holds %d, which is its ceiling, so close one first", ErrCeilingReached, s.maxSessions())
	}
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	conv := newConversation(id, svc)
	s.sessions[id] = conv
	return conv, nil
}

// createSession mints a conversation through the configured factory.
func (s *Server) createSession() (*conversation, error) {
	if s.opts.NewService == nil {
		// Honest rather than clever: a gateway started without a way to build a conversation
		// serves one, and saying so beats minting a conversation whose service would be the
		// same one the first conversation already has - which would be two names for one
		// transcript.
		return nil, errors.New("this gateway serves one conversation: it was started without a way to build another")
	}
	svc, err := s.opts.NewService()
	if err != nil {
		return nil, fmt.Errorf("a conversation could not be started: %w", err)
	}
	return s.newSession(svc)
}

// newSessionID returns an unguessable id for one conversation.
//
// Random rather than sequential, for the same reason the approval id is: a client that can guess
// another's id can address another's conversation. The token is what actually protects this
// gateway, so a guessable id would not be a hole - but the id is also how a client keeps itself
// from confusing two conversations, and a name that cannot collide by accident costs nothing.
func newSessionID() (string, error) {
	b := make([]byte, 12)
	if _, err := io.ReadFull(randReader, b); err != nil {
		return "", fmt.Errorf("could not form a session id: %w", err)
	}
	return "s" + hex.EncodeToString(b), nil
}

// handleCreateSession mints one more conversation and answers with it.
//
// 201 with the id, and the id is the only thing a client needs: every other endpoint is addressed
// by it, so a client that loses one opens another.
//
// The two refusals are told apart. A gateway that cannot build conversations at all is 501 - the
// capability is absent. A gateway that is full is 409 - the capability is there and the request is
// the one that cannot be served yet, and a client that reads 409 can close a session and retry
// where a 501 tells it to give up.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProjectID string `json:"project_id"`
	}
	// The body is optional: a client that sends no body gets a free-standing session.
	if r.ContentLength > 0 {
		if !s.decodeBody(w, r, &body) {
			return
		}
	}

	// When a project is named, the session runs in that project's workspace.
	// The project must exist: a session for a missing project would run in
	// the wrong directory, which is exactly what the project feature prevents.
	//
	// For a git project the session gets its OWN worktree rather than the
	// project's checkout: two sessions in one project would otherwise edit the
	// same files. The worktree is best-effort - see sessionWorktree - so a
	// project that is not a repository still gives a usable session.
	var project *Project
	if strings.TrimSpace(body.ProjectID) != "" {
		project = s.projectOf(body.ProjectID)
		if project == nil {
			writeError(w, http.StatusNotFound, ErrProjectNotFound.Error())
			return
		}
	}

	conv, err := s.createSession()
	switch {
	case errors.Is(err, ErrCeilingReached):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusNotImplemented, err.Error())
	default:
		if project != nil {
			// The worktree is made BEFORE the session is saved, so a session
			// that is registered is one that can actually run. A failure to
			// branch falls back to the project's own directory.
			dir := project.Dir
			if wt, wtErr := s.sessionWorktree(context.Background(), project.Dir, conv.id); wtErr != nil {
				if s.opts.Log != nil {
					s.opts.Log.Warn("the session will run in the project directory: its own worktree could not be created",
						"id", conv.id, "project", project.Dir, "error", wtErr.Error())
				}
			} else {
				dir = wt
			}
			conv.setProjectID(body.ProjectID, dir, project.Dir)
			conv.svc.SetWorkspace(dir)
		}
		s.saveSession(conv)
		writeJSON(w, http.StatusCreated, conv.status())
	}
}

// handleListSessions answers what this gateway is holding.
//
// Sorted by id so that a client reading it twice sees the same order, and so does a test.
func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	all := s.snapshot()
	out := make([]SessionStatus, 0, len(all))
	for _, c := range all {
		out = append(out, c.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleDeleteSession drops one conversation.
//
// The default one cannot be removed — it belongs to the process that started this
// gateway — but deleting it is treated as a reset: the transcript is cleared, the
// title is restored to the "New session" placeholder, and the persisted file is
// removed. The session stays alive but empty, which is what a user who presses
// "delete" on it expects.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	c := convOf(r)
	// A run in flight is STOPPED, not refused. Deleting a conversation is a
	// decision about that conversation, and the old answer - a 409 telling the
	// user to come back after the run finishes - made the decision conditional on
	// a turn that may run for minutes, with no way to end it from here.
	//
	// The two halves have to be in this order and both have to happen: the
	// cancellation kills what the agent is RUNNING (the sandbox process group, a
	// pending approval, the model call), and the wait makes sure the goroutine
	// has finished unwinding - it still appends its final events, generates an
	// auto-title and saves the session - before the conversation is forgotten and
	// its file removed. Deleting first would leave a turn writing into a session
	// that no longer exists.
	if stopped, settled := c.stopRunForDeletion(deleteStopTimeout); stopped && !settled {
		writeError(w, http.StatusConflict,
			"the run in this session did not stop in time, so the session was not deleted: stop it and try again")
		return
	}
	if c.id == DefaultSession {
		c.svc.ResetConversation()
		c.setTitle(placeholderTitle)
		s.deletePersistedSession(c.id)
		writeJSON(w, http.StatusOK, c.status())
		return
	}
	// No "was it there?" branch: withConversation already proved it is, and a concurrent second
	// DELETE of the same session would find it gone - which is the outcome both callers asked
	// for. Reporting 404 to one of them would be reporting a race, not a fact about the session.
	s.forget(c.id)
	s.deletePersistedSession(c.id)
	w.WriteHeader(http.StatusNoContent)
}

// handleRenameSession changes the human-readable title of a conversation.
//
// 200 with the updated status rather than 204: a client that renamed a session draws the new
// title from the response, and a second round-trip to fetch it would be a race with any other
// client editing the same session.
func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if !s.decodeBody(w, r, &body) {
		return
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "the title cannot be empty")
		return
	}
	c := convOf(r)
	c.setTitle(title)
	s.saveSession(c)
	writeJSON(w, http.StatusOK, c.status())
}

// maybeAutoTitle sets a title generated by the LLM when the conversation still
// has an unchosen placeholder title. It is called after a turn
// completes, so a session that was just created gets a human-readable label
// without the user naming it themselves.
func (s *Server) maybeAutoTitle(c *conversation) {
	// Only replace a placeholder title, never a user-set or already-generated one.
	if !isPlaceholderTitle(c.status().Title) {
		return
	}
	turns := c.svc.Transcript()
	for _, t := range turns {
		if t.User != "" {
			title := c.svc.GenerateTitle(context.Background(), t.User)
			if title != "" {
				c.setTitle(title)
				s.saveSession(c)
			}
			return
		}
	}
}
