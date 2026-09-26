package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madkoding/motita/internal/agent"
	"github.com/madkoding/motita/internal/config"
	"github.com/madkoding/motita/internal/logx"
	"github.com/madkoding/motita/internal/netrules"
	"github.com/madkoding/motita/internal/session"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeService is a Service whose every answer a test chooses.
//
// It exists so the transport can be tested without running an agent: a test that needs an LLM
// to check a status code is a test that fails for the wrong reason, and it would be slow enough
// that people stop running it.
type fakeService struct {
	plan      func(ctx context.Context, prompt string, progress func(string, ...any)) (string, error)
	task      func(ctx context.Context, task string, progress func(string, ...any)) (string, error)
	models    func(ctx context.Context) (string, error)
	report    string
	summary   session.Snapshot
	reset     int
	cfg       config.Config
	reasoning string
	verdict   func(good bool, note string) string
	reward    string
	questions []agent.AskItem
	origin    string
	// transcript is the conversation the service holds. It is what the read endpoint publishes,
	// so a test can put a conversation there and read it back over HTTP.
	transcript []agent.DialogueTurn
	approver   agent.Approver
	// approverWrap is called by SetApprover, so a test can reach the approver the SERVER
	// installed without the service having to hand it back. The server owns that wiring and the
	// test only observes it.
	approverWrap func(agent.Approver)
}

func (f *fakeService) RunPlan(ctx context.Context, p string, pr func(string, ...any)) (string, error) {
	if f.plan == nil {
		return "", nil
	}
	return f.plan(ctx, p, pr)
}

func (f *fakeService) RunTask(ctx context.Context, t2 string, pr func(string, ...any)) (string, error) {
	if f.task == nil {
		return "", nil
	}
	return f.task(ctx, t2, pr)
}

func (f *fakeService) RunModels(ctx context.Context) (string, error) {
	if f.models == nil {
		return f.report, nil
	}
	return f.models(ctx)
}

func (f *fakeService) ConversationReport() string            { return f.report }
func (f *fakeService) ConversationSummary() session.Snapshot { return f.summary }
func (f *fakeService) ResetConversation()                    { f.reset++ }
func (f *fakeService) Config() config.Config                 { return f.cfg }
func (f *fakeService) SetReasoning(level string)             { f.reasoning = level }
func (f *fakeService) SetLLM(provider, model string) {
	if provider != "" {
		f.cfg.LLM.Provider = provider
	}
	if model != "" {
		f.cfg.LLM.Model = model
	}
}

func (f *fakeService) RecordVerdict(good bool, note string) string {
	if f.verdict == nil {
		return "recorded"
	}
	return f.verdict(good, note)
}

func (f *fakeService) RewardReport() string { return f.reward }

func (f *fakeService) TakePendingQuestions() ([]agent.AskItem, string) {
	items, origin := f.questions, f.origin
	f.questions, f.origin = nil, ""
	return items, origin
}

func (f *fakeService) SetApprover(fn agent.Approver) {
	f.approver = fn
	if f.approverWrap != nil {
		f.approverWrap(fn)
	}
}

// Transcript answers the conversation so far. It hands out a COPY, exactly like the production
// runner does: a caller that mutated the slice it was given would be editing the conversation.
func (f *fakeService) Transcript() []agent.DialogueTurn {
	return append([]agent.DialogueTurn(nil), f.transcript...)
}

// RestoreTranscript loads a saved conversation into the fake, replacing what it holds.
func (f *fakeService) RestoreTranscript(turns []agent.DialogueTurn) {
	f.transcript = append([]agent.DialogueTurn(nil), turns...)
}

func (f *fakeService) SetWorkspace(dir string) {
	if dir != "" {
		f.cfg.Agent.WorkspaceDir = dir
	}
}

func (f *fakeService) GenerateTitle(_ context.Context, firstUserMessage string) string {
	// Mirror the fallback: truncate the first user message.
	return strings.Join(strings.Fields(firstUserMessage), " ")
}

// newTestServer starts a REAL listener on loopback with an ephemeral port.
//
// Real HTTP over a real socket, because a handler test that never binds cannot see the parts
// that only exist once bytes are on a wire: streaming, flushing, a half-closed connection. The
// port is 0 so two tests in this package can never fight over one.
func newTestServer(t *testing.T, svc Service, mutators ...func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Service:   svc,
		Listen:    "127.0.0.1:0",
		Token:     testToken,
		Version:   "test",
		MaxBodyKB: defaultMaxBodyKB,
	}
	for _, m := range mutators {
		m(&opts)
	}
	srv, err := Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	return srv
}

// waitForNoRun blocks until no conversation on the server is running.
//
// It exists so a test's temp directories are quiet before t.TempDir's cleanup removes
// them: a run that has just been released still writes, and that write racing RemoveAll
// is a flake that fails about one run in twenty under load.
func waitForNoRun(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		running := false
		for _, c := range srv.snapshot() {
			if c.isRunning() {
				running = true
			}
		}
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("a run never finished, so its writer can still race the temp dir")
}

// get performs a request against the server and returns the recorder.
func get(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.BaseURL()+path, nil)
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

func TestHealthAnswersWithoutAToken(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := get(t, srv, "/v1/health", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a client must be able to tell 'nothing there' from 'wrong token'", w.Code)
	}
	var got struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body = %q: %v", w.Body.String(), err)
	}
	if !got.OK || got.Version != "test" {
		t.Errorf("health = %+v", got)
	}
}

func TestEveryOtherEndpointNeedsTheToken(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	paths := []string{
		sessionPath(srv, DefaultSession, "/config"), sessionPath(srv, DefaultSession, ""), sessionPath(srv, DefaultSession, "/report"), sessionPath(srv, DefaultSession, "/models"),
		sessionPath(srv, DefaultSession, "/reward"), sessionPath(srv, DefaultSession, "/questions"),
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			if w := get(t, srv, path, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("%s without a token = %d, want 401", path, w.Code)
			}
			if w := get(t, srv, path, testToken); w.Code == http.StatusUnauthorized {
				t.Errorf("%s with the token must not be 401", path)
			}
		})
	}
	for _, path := range []string{sessionPath(srv, DefaultSession, "/reset"), sessionPath(srv, DefaultSession, "/reasoning"), sessionPath(srv, DefaultSession, "/verdict"), sessionPath(srv, DefaultSession, "/task"), sessionPath(srv, DefaultSession, "/plan"), sessionPath(srv, DefaultSession, "/runs/approval")} {
		t.Run("POST "+path, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+path, strings.NewReader("{}"))
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("POST %s without a token = %d, want 401", path, w.Code)
			}
		})
	}
}

func TestConfigEndpointCarriesNoKey(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.APIKey = "sk-canary-0123456789"
	cfg.LLM.Provider = "openai"
	srv := newTestServer(t, &fakeService{cfg: cfg})

	w := get(t, srv, sessionPath(srv, DefaultSession, "/config"), testToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-canary") {
		t.Fatalf("the API key was served: %s", w.Body.String())
	}
	var v configView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("body = %q: %v", w.Body.String(), err)
	}
	if !v.APIKeyPresent || v.Provider != "openai" {
		t.Errorf("view = %+v", v)
	}
}

func TestABindFailureIsReported(t *testing.T) {
	first := newTestServer(t, &fakeService{})
	// The same address, taken. A silent fallback to a different port would make a client that
	// was told one address talk to a server that is somewhere else.
	_, err := Start(Options{Service: &fakeService{}, Listen: first.Addr(), Token: testToken})
	if err == nil {
		t.Fatal("binding an address that is in use must be reported")
	}
	if !strings.Contains(err.Error(), "could not listen") {
		t.Errorf("err = %v, it must say what failed", err)
	}
}

func TestABadListenAddressIsReported(t *testing.T) {
	_, err := Start(Options{Service: &fakeService{}, Listen: "not an address", Token: testToken})
	if err == nil {
		t.Fatal("an address that cannot be parsed must be reported")
	}
	if !strings.Contains(err.Error(), "host:port") {
		t.Errorf("err = %v, it must say the shape it wanted", err)
	}
}

// A non-loopback listen is ACCEPTED with nothing else set, because the second act is gone.
//
// This is the test that has to break first if anyone reintroduces the old posture. The previous
// design required `allow_lan` here, and the property worth pinning is not the absence of a check but
// what replaced it: WHO MAY CONNECT is now a rule applied per request, so an address on every
// interface is a statement about the SOCKET and nothing else.
func TestANonLoopbackAddressNeedsNoOtherSetting(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.Listen = "0.0.0.0:0" })
	if !srv.ReachableFromNetwork() {
		t.Fatal("a wildcard bind must report itself as reachable from the network")
	}
	// And it actually answers, which is the part a validation-only test would miss.
	resp, err := http.Get(srv.BaseURL() + "/v1/health")
	if err != nil {
		t.Fatalf("the announced address %q cannot be reached: %v", srv.BaseURL(), err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health answered %d, want 200", resp.StatusCode)
	}
}

// The origin rules are applied to EVERY request, and the refusal names the rule set.
//
// The request is driven through a real socket so that RemoteAddr is a real client address rather
// than the empty string a hand-built httptest.Request carries: a test that injects RemoteAddr is a
// test of the matcher, and the matcher already has its own tests in internal/netrules. What is
// checked HERE is that the middleware is wired around the routing table and reads the connection.
//
// The test uses a loopback client against a `!any` rule set, which is the one combination that can
// refuse something from inside a test process: loopback is exempt from the rules, so a rule set of
// `lan` would accept this client and prove nothing.
func TestAnOriginOutsideTheRulesIsRefused(t *testing.T) {
	policy, err := netrules.Parse([]string{"!any"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.Allow = policy })

	// A loopback client is EXEMPT, whatever the rules say: the local interface and the local
	// browser reach the gateway that way, and a rule set that locked it out would leave the
	// operator unable to repair what they just configured.
	resp, err := http.Get(srv.BaseURL() + "/v1/health")
	if err != nil {
		t.Fatalf("loopback must always be served: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback answered %d under `!any`, want 200: loopback is always allowed", resp.StatusCode)
	}

	// A non-loopback client is refused, and this is asserted on the middleware directly because
	// the test process cannot originate from another address. The RemoteAddr is the ONLY thing
	// faked; everything else goes through the real handler chain.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = "192.168.100.90:41234"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a refused origin answered %d, want 403", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "gateway.allow") {
		t.Errorf("the refusal does not name the setting to edit: %s", body)
	}
	if !strings.Contains(body, "192.168.100.90") {
		t.Errorf("the refusal does not name the origin it refused: %s", body)
	}
}

// A refused origin does not learn whether its credential was good.
//
// The policy runs BEFORE the token check, and that order is the point: a gateway restricted to one
// office must not answer "your token is wrong" to a machine in another country, which would confirm
// the gateway is there and that guessing credentials is worth trying.
func TestARefusedOriginIsRefusedBeforeTheTokenIsChecked(t *testing.T) {
	policy, err := netrules.Parse([]string{"!any"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.Allow = policy })

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.RemoteAddr = "10.0.0.5:5555"
	// A WRONG token, deliberately: a 403 rather than a 401 is the proof that the origin was
	// judged first.
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("00", 32))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a refused origin with a bad token answered %d, want 403 (the origin is judged first)", w.Code)
	}
}

// An origin claiming to be allowed through a header is NOT believed.
//
// This is the whole security property of the middleware and the reason it reads RemoteAddr: if a
// header were honoured, anybody could reach a gateway restricted to one machine by claiming to be
// it, and the allow list would be a decoration.
func TestAForwardedHeaderCannotClaimAnAllowedOrigin(t *testing.T) {
	policy, err := netrules.Parse([]string{"!any"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.Allow = policy })

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.RemoteAddr = "203.0.113.9:12345"
	// Every header a proxy would set, all claiming to be a machine the rules WOULD allow.
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "127.0.0.1")
	req.Header.Set("Forwarded", "for=127.0.0.1")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a forwarded header was believed: answered %d, want 403", w.Code)
	}
}

// An EMPTY rule set, and a nil one, both serve every origin: two spellings of the documented
// default must not behave differently.
func TestNoRulesMeansEveryOrigin(t *testing.T) {
	empty, err := netrules.Parse(nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, policy := range map[string]*netrules.Policy{"nil": nil, "empty": empty} {
		srv := newTestServer(t, &fakeService{}, func(o *Options) { o.Allow = policy })

		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("with a %s rule set an arbitrary origin answered %d, want 200: the default policy is accept", name, w.Code)
		}
	}
}

// An origin that cannot be ESTABLISHED is refused, not assumed benign.
//
// This is the defensive branch of the middleware, and it refuses on purpose: a request whose origin
// cannot be parsed is exactly the one not to guess about. Left as a branch nothing runs, its
// behaviour would be whatever a later edit happened to write; here it is pinned.
func TestAnUnreadableOriginIsRefused(t *testing.T) {
	policy, err := netrules.Parse([]string{"!any"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.Allow = policy })

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	// RemoteAddr is "ip:port" on a real socket. Anything else cannot be judged, and "cannot be
	// judged" must not mean "let it in".
	req.RemoteAddr = "not-an-address"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("an origin that cannot be parsed answered %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "origin") {
		t.Errorf("the refusal does not say that the origin could not be determined: %s", w.Body.String())
	}
}

// The gateway SAYS what rules it is enforcing, because that description is written to the service
// file and read back by a later `status` or `stop`.
func TestTheGatewayDescribesItsOwnRules(t *testing.T) {
	// No policy at all describes itself as accepting everything - and through the policy's own
	// words rather than a second phrase, so the two spellings cannot drift.
	bare := newTestServer(t, &fakeService{})
	if got := bare.AllowDescription(); got != (&netrules.Policy{}).Describe() {
		t.Errorf("a gateway with no rules describes itself as %q, want the open policy's own words", got)
	}

	policy, err := netrules.Parse([]string{"lan", "!any"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	strict := newTestServer(t, &fakeService{}, func(o *Options) { o.Allow = policy })
	if got := strict.AllowDescription(); got != policy.Describe() {
		t.Errorf("the gateway describes its rules as %q, want %q: the description must be the policy's own", got, policy.Describe())
	}
	// And the description is not the open one, which is the assertion that would catch a gateway
	// reporting the default while enforcing something else.
	if strict.AllowDescription() == bare.AllowDescription() {
		t.Fatal("a restricted gateway describes itself exactly like an open one")
	}
}

func TestAnEmptyTokenRefusesToStart(t *testing.T) {
	for _, token := range []string{"", "   "} {
		_, err := Start(Options{Service: &fakeService{}, Listen: "127.0.0.1:0", Token: token})
		if err == nil {
			t.Fatalf("the token %q must be refused: an unauthenticated agent is a remote shell", token)
		}
	}
}

func TestNoServiceRefusesToStart(t *testing.T) {
	if _, err := Start(Options{Listen: "127.0.0.1:0", Token: testToken}); err == nil {
		t.Fatal("a gateway with nothing to speak for must be refused")
	}
}

func TestAnEmptyListenMeansLoopbackEphemeral(t *testing.T) {
	srv, err := Start(Options{Service: &fakeService{}, Token: testToken})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Close(context.Background()) }()
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Errorf("addr = %q, want loopback", srv.Addr())
	}
	if !strings.HasPrefix(srv.BaseURL(), "http://127.0.0.1:") {
		t.Errorf("base URL = %q", srv.BaseURL())
	}
	if srv.Token() != testToken {
		t.Errorf("token = %q", srv.Token())
	}
}

func TestTheDefaultBodyCapIsApplied(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.MaxBodyKB = 0 })
	if srv.opts.MaxBodyKB != defaultMaxBodyKB {
		t.Errorf("max body = %d, want the default %d", srv.opts.MaxBodyKB, defaultMaxBodyKB)
	}
}

func TestAnUnknownPathIsNotFound(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	if w := get(t, srv, "/v1/nope", testToken); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestAWrongMethodIsNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	// GET on a POST-only route: Go's method-aware routing answers 405, which is what tells a
	// client the path exists and the verb is wrong.
	req, _ := http.NewRequest(http.MethodGet, srv.BaseURL()+sessionPath(srv, DefaultSession, "/task"), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestAStreamLivesLongerThanTheDefaultTimeouts(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	if srv.server.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v; a streamed run must not be cut off by it", srv.server.WriteTimeout)
	}
	if srv.server.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout must be set: without it a stalled client holds a connection forever")
	}
}

func TestCloseIsIdempotentAndShutsTheListener(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	addr := srv.Addr()
	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The port is free again: a test that leaks its listener makes the next one flaky.
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/health", nil)
	if _, err := (&http.Client{Timeout: 2 * time.Second}).Do(req); err == nil {
		t.Error("the listener is still accepting after Close")
	}
}

func TestIsLoopbackKnowsItsAddresses(t *testing.T) {
	cases := map[string]bool{
		"":            false,
		"localhost":   true,
		"LOCALHOST":   true,
		"127.0.0.1":   true,
		"::1":         true,
		"0.0.0.0":     false,
		"192.168.1.5": false,
		"example.org": false,
	}
	for host, want := range cases {
		if got := isLoopback(host); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

// The placeholder test is GONE rather than updated: every endpoint it covered is now real, and a
// test that pins 501 for a written handler is a test that would have to be deleted to let the
// handler work. It existed to prove the routes were behind the token while the work was in
// progress; TestEveryOtherEndpointNeedsTheToken does that permanently.

// --- event framing ----------------------------------------------------------

func TestMarshalEventReportsWhatCannotBeEncoded(t *testing.T) {
	// A channel cannot be marshalled. It is the one way to reach the branch, and the branch is
	// worth having: it is what turns an encoder failure into an error a caller can report
	// instead of a silent empty event.
	if _, err := marshalEvent(make(chan int)); err == nil {
		t.Fatal("an unencodable payload must be reported")
	}
}

func TestWriteEventFramesOneDataLine(t *testing.T) {
	w := httptest.NewRecorder()
	rc := http.NewResponseController(w)
	if err := writeEvent(w, rc, 7, EventProgress, progressEvent{Text: "a line"}); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	body := w.Body.String()
	// The `id:` line carries the SEQUENCE NUMBER, which is what a client sends back to resume.
	// Asserting the whole frame and not just its presence is what keeps the wire format from
	// drifting one field at a time.
	if body != "id: 7\nevent: progress\ndata: {\"text\":\"a line\"}\n\n" {
		t.Errorf("body = %q", body)
	}
	// The invariant the client depends on: exactly one line carrying data, so it can dispatch
	// on it without buffering multi-line events.
	if n := strings.Count(body, "\ndata: "); n != 1 {
		t.Errorf("the event carries %d data lines, want exactly 1", n)
	}
}

func TestWriteEventReportsAnUnencodablePayload(t *testing.T) {
	w := httptest.NewRecorder()
	rc := http.NewResponseController(w)
	if err := writeEvent(w, rc, 0, EventError, make(chan int)); err == nil {
		t.Fatal("an unencodable payload must be reported")
	}
}

func TestWriteEventReportsAFailedWrite(t *testing.T) {
	// A client that went away mid-stream. The error is what the caller turns into "stop
	// streaming"; swallowing it would keep a run producing lines for nobody.
	rc := http.NewResponseController(failingWriter{})
	err := writeEvent(failingWriter{}, rc, 0, EventProgress, progressEvent{Text: "x"})
	if err == nil {
		t.Fatal("a failed write must be reported")
	}
}

func TestStartStreamReportsAFailedWrite(t *testing.T) {
	rc := http.NewResponseController(failingWriter{})
	if err := startStream(failingWriter{}, rc); err == nil {
		t.Fatal("a failed write must be reported")
	}
}

func TestStartStreamSetsTheHeadersAStreamNeeds(t *testing.T) {
	w := httptest.NewRecorder()
	rc := http.NewResponseController(w)
	if err := startStream(w, rc); err != nil {
		t.Fatalf("startStream: %v", err)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Errorf("content type = %q", ct)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cache control = %q", got)
	}
	if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("a buffering proxy would turn this stream into a hang: %q", got)
	}
	// The opening comment is what gets the headers on the wire before the first event, so a
	// client waiting for its first byte is not left wondering whether it was even accepted.
	if !strings.HasPrefix(w.Body.String(), ": connected\n\n") {
		t.Errorf("body = %q", w.Body.String())
	}
}

// failingWriter is a connection that has gone away.
type failingWriter struct{}

func (failingWriter) Header() http.Header       { return http.Header{} }
func (failingWriter) WriteHeader(int)           {}
func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the client is gone") }

// noFlushWriter is a ResponseWriter with no Flush, so the ResponseController cannot take the
// stream over. A recorder has one; this does not, which is how the error path is reached.
type noFlushWriter struct{}

func (noFlushWriter) Header() http.Header         { return http.Header{} }
func (noFlushWriter) WriteHeader(int)             {}
func (noFlushWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestFlushingAnUnflushableWriterIsReported(t *testing.T) {
	rc := http.NewResponseController(noFlushWriter{})
	if err := rc.Flush(); err == nil {
		t.Fatal("flushing a writer that cannot flush must be an error, not silence")
	}
}

// Compile-time: the writers above really are http.ResponseWriters.
var (
	_ http.ResponseWriter = failingWriter{}
	_ http.ResponseWriter = noFlushWriter{}
)

// --- the body reader and the serve loop --------------------------------------

// decodeBody is exercised directly because in this slice nothing routes through it yet: the
// handlers that take a body arrive with the runs. Testing it now is what keeps the framing of a
// refusal (a JSON {"error":...} at 400) pinned before six handlers depend on it.
func TestDecodeBodyAcceptsTheExpectedJSON(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/anything", strings.NewReader(`{"level":"high"}`))
	var got struct {
		Level string `json:"level"`
	}
	if !srv.decodeBody(w, r, &got) {
		t.Fatalf("a well-formed body was refused: %s", w.Body.String())
	}
	if got.Level != "high" {
		t.Errorf("level = %q", got.Level)
	}
}

func TestDecodeBodyRefusesWhatIsNotJSON(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	for _, body := range []string{"not json", `{"level":`, ""} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/anything", strings.NewReader(body))
		if srv.decodeBody(w, r, &struct{}{}) {
			t.Errorf("the body %q was accepted", body)
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("the body %q got status %d, want 400", body, w.Code)
		}
		if !strings.Contains(w.Body.String(), "not the JSON") {
			t.Errorf("the refusal must say what it wanted: %s", w.Body.String())
		}
	}
}

// An oversized body is refused, and it is refused as a 400 with a reason rather than a
// truncated read that would look like a malformed request with no explanation. MaxBytesReader
// makes the decode fail, which is the single path a client sees.
func TestDecodeBodyRefusesAnOversizedBody(t *testing.T) {
	srv := newTestServer(t, &fakeService{}, func(o *Options) { o.MaxBodyKB = 1 })
	w := httptest.NewRecorder()
	big := `{"task":"` + strings.Repeat("x", 4096) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/anything", strings.NewReader(big))
	if srv.decodeBody(w, r, &struct{}{}) {
		t.Fatal("a body over the cap was accepted")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// Serve announces where it is listening. That log line is the ONLY way an operator learns the
// port when 0 was asked for, so it is asserted rather than assumed: the logger writes to a file
// here because that is the only destination logx.Options exposes, and reading the file back is
// how the announcement is checked.
func TestServeReportsWhereItIsListening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	log, err := logx.New(logx.Options{Path: path, Level: logx.Info})
	if err != nil {
		t.Fatalf("building the logger: %v", err)
	}

	srv, err := Start(Options{Service: &fakeService{}, Listen: "127.0.0.1:0", Token: testToken, Log: log})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()

	// The listener is up before Serve is called (Start binds), so the announcement follows
	// immediately; the wait is for the write to reach the file, not for the bind.
	addr := srv.Addr()
	var body []byte
	for i := 0; i < 200; i++ {
		body, _ = os.ReadFile(path)
		if strings.Contains(string(body), addr) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(body), addr) {
		t.Fatalf("the gateway never announced where it is listening (log = %q)", body)
	}

	// A closed listener is how this server STOPS, so it is not an error.
	if err := srv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve reported %v after a clean Close, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}

// A listener that fails for a reason OTHER than being closed is reported: that is a real
// failure and swallowing it would make the gateway look healthy while it serves nobody.
func TestServeReportsARealListenerFailure(t *testing.T) {
	srv := newTestServer(t, &fakeService{})
	// Close the listener behind the server's back, which is what a failure at the accept
	// syscall looks like from here: it is not ErrServerClosed, so it must come back.
	if err := srv.listener.Close(); err != nil {
		t.Fatalf("closing the fixture listener: %v", err)
	}
	if err := srv.Serve(); err == nil {
		t.Error("a listener that stopped for a reason other than Close must be reported")
	}
}

// The gateway answers no cross-origin request, and this is a DECISION made executable rather than
// an accident of configuration.
//
// A browser cannot keep the bearer token secret - the client is source code anyone can read, and
// any XSS reads localStorage - so a page that could reach this gateway would be a page that could
// drive an agent that runs commands on this machine with the user's credentials. The same absence
// also protects against a page the user merely has open: without an Access-Control-Allow-Origin
// header the browser blocks the response before the page can read it.
//
// The web front end therefore talks to its OWN proxy, which holds the token server-side. This test
// exists so that nobody adds `Access-Control-Allow-Origin: *` "to make the web client work": that
// change compiles, makes the immediate problem go away, and hands the gateway to every page the
// user has open. It has to break a test that says why.
func TestTheGatewayAnswersNoCrossOriginRequest(t *testing.T) {
	srv := newTestServer(t, &fakeService{})

	// A preflight, which is what a browser sends before a real cross-origin call.
	req, err := http.NewRequest(http.MethodOptions, srv.BaseURL()+"/v1/health", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Origin", "https://an-evil-page.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("the gateway answered a preflight with Access-Control-Allow-Origin: %q. "+
			"A browser holding this header can drive an agent that runs commands on this "+
			"machine; the web front end must go through its own proxy instead", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials: %q", got)
	}

	// And on an ordinary authenticated request, with an Origin present, which is the shape a
	// simple cross-origin POST has when no preflight is triggered.
	req2, err := http.NewRequest(http.MethodGet, srv.BaseURL()+"/v1/health", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req2.Header.Set("Origin", "https://an-evil-page.example")
	resp2, err := (&http.Client{}).Do(req2)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp2.Body.Close()
	for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers"} {
		if got := resp2.Header.Get(h); got != "" {
			t.Errorf("the gateway answered %s on a request carrying an Origin: %q", h, got)
		}
	}
	// The request itself is answered: refusing the CORS header is not refusing the API. A native
	// client ignores all of this and keeps working.
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("health answered %d; the absence of CORS must not break non-browser clients", resp2.StatusCode)
	}
}
