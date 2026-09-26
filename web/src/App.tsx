import { useState, useEffect, useRef, useCallback, useMemo } from 'preact/hooks'
import { Markdown } from './Markdown'

interface Message {
  id: number
  role: 'user' | 'agent' | 'activity'
  text: string
  kind?: string
}

interface PendingApproval {
  id: string
  question?: string
  reason?: string
  command?: string
}

interface SessionInfo {
  id: string
  title: string
  project_id?: string
  branch?: string
  mergeable?: boolean
  changes?: number
  worktree?: string
  created: string
  last_used: string
  running: boolean
}

interface ProjectInfo {
  id: string
  title: string
  description?: string
  dir: string
  git_url?: string
  branch?: string
  changes?: number
  sessions?: number
  worktrees?: number
  created: string
}

// shortID abbreviates a generated id the way git abbreviates a sha: the first
// seven characters. A full id is 25 characters and would be the widest thing in
// the sidebar while telling the reader nothing the first seven do not.
function shortID(id: string): string {
  return id.length > 7 ? id.slice(0, 7) : id
}

// shortBranch keeps a branch name readable when it is one this program
// generated. `motita/<id>` is the session's OWN branch and the id is the only
// part that varies, so it is shown as `motita/<first 7>`; a branch the user
// chose is short and is shown exactly as it is.
function shortBranch(b: string): string {
  const m = b.match(/^motita\/(.+)$/)
  if (m && m[1].length > 7) return 'motita/' + shortID(m[1])
  return b
}

// sessionWorktreeChip decides whether a session's worktree needs saying at all.
//
// A session on its own branch IS in the worktree that branch names, so printing
// the id twice is one fact twice: `motita/s63bb92` beside `⌥ s63bb92`. The two
// differ in exactly one case - the user moved the session to another branch -
// and that is the case where the worktree is the only way to tell which session
// the branch belongs to.
function sessionWorktreeChip(s: SessionInfo): string {
  if (!s.worktree || !s.branch) return ''
  return s.branch === 'motita/' + s.worktree ? '' : shortID(s.worktree)
}

// formatUpdated renders when a session was last used, as dd/mm/yyyy :: HH:mm:ss.
//
// Seconds and the full year are carried even though a sidebar usually shows
// something shorter, because the user asked for this exact format and a
// timestamp is a fact: two sessions opened a minute apart are told apart by the
// seconds, and the year is what keeps a session restored from a backup from
// reading as if it were touched today.
function formatUpdated(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getDate())}/${p(d.getMonth() + 1)}/${d.getFullYear()} :: ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

// fullUpdated is the exact time, for the tooltip. The row carries the compact
// form because that is what fits; hovering is how a user asks "when, exactly",
// and the answer must not be the truncated one they can already see.
function fullUpdated(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
}

// changesTitle is what a change count means, spelled out: a bare number in a
// badge is read as fact, so the tooltip says what it counted.
function changesTitle(n: number): string {
  if (n === 1) return '1 uncommitted change in this working tree'
  return `${n} uncommitted changes in this working tree`
}

interface ConfigView {
  provider: string
  model: string
  reasoning: string
  reasoning_enabled: boolean
  api_key_present: boolean
}

interface ProviderInfo {
  id: string
  name: string
  models: string[]
  fetch_models: boolean
  key_present: boolean
  is_current: boolean
}

interface ScheduledTask {
  id: string
  title: string
  task: string
  kind: string
  session_id: string
  every: string
  enabled: boolean
  created: string
  last_run?: string
  last_outcome?: string
  run_count: number
  next_run: string
}

const STORAGE_KEY = 'motita:last-session'
const SIDEBAR_KEY = 'motita:sidebar-open'
const PROJECT_COLLAPSE_KEY = 'motita:collapsed-projects'
const UPGRADE_DISMISS_KEY = 'motita:upgrade-dismissed'

// ─── Scheduled tasks: reading a cadence in words, and a countdown ────────────
//
// `every` arrives from the gateway as a Go duration string (the DTO formats it),
// so the units are known: the tag reads "Every 24h", never a second, invented
// spelling of the same cadence.

// countdown is how long is left before the next run, as a chronometer:
// mm:ss under an hour, h:mm:ss under a day, days and hours beyond that. The
// seconds field is why this ticks at all - a countdown that only moved once a
// minute would look frozen on the run that matters, the imminent one. Past a day
// the seconds are dropped because they would be noise, and the panel re-renders
// on the same interval either way.
function pad(n: number): string {
  return n < 10 ? '0' + n : String(n)
}

function countdown(fromMs: number, nowMs: number): { text: string; past: boolean } {
  const delta = Math.round((fromMs - nowMs) / 1000)
  const past = delta < 0
  const s = Math.abs(delta)
  let text: string
  if (s < 3600) text = Math.floor(s / 60) + ':' + pad(s % 60)
  else if (s < 86400) text = Math.floor(s / 3600) + ':' + pad(Math.floor((s % 3600) / 60)) + ':' + pad(s % 60)
  else text = Math.floor(s / 86400) + 'd ' + Math.floor((s % 86400) / 3600) + 'h'
  return { text, past }
}

// durationOf reads a Go duration string ("24h0m0s") twice over, which is the
// only reason it is one function: the cadence is needed as MILLISECONDS (for the
// clock skew below) and as the SHORT form a person typed (for the row's tag).
// Two parsers over the same grammar is two things to keep in step, and the
// normalised form the gateway sends is not what belongs on a tag - "24h0m0s" is
// a machine's spelling of "24h". A cadence that cannot be read comes back with
// the text it arrived as, which is more useful than an empty tag on a row that
// does have one.
const DURATION_UNITS: Record<string, number> = {
  ns: 1e-6, us: 1e-3, 'µs': 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000,
}

function durationOf(every: string): { ms: number; short: string } {
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g
  let ms = 0
  let short = ''
  let match: RegExpExecArray | null
  while ((match = re.exec(every)) !== null) {
    const n = parseFloat(match[1])
    ms += n * DURATION_UNITS[match[2]]
    // Only whole units reach the tag: "0h" out of "24h0m0s" is noise, and the
    // unit order the parser walks is the order Go prints, h first.
    if (n > 0) short += match[1] + match[2]
  }
  return { ms, short: short || every }
}

// nextClockSkew is the skew assumed when nothing can be derived from the tasks:
// zero, so the countdown is exactly what the gateway's timestamps say. It only
// applies to a task set that is empty or unreadable, where the correction has
// nothing to correct.
const nextClockSkew = 0

// taskCountdown answers how long until this task fires, as the panel shows it.
// A paused task counts down to nothing: its next_run is a projection, and a
// ticker over a task that will not fire would be a lie with a pulse. An overdue
// one says so, because "0s" is not the same fact as "it should have gone off".
function taskCountdown(t: ScheduledTask, nowMs: number, skew: number) {
  if (!t.enabled) return null
  const ts = Date.parse(t.next_run)
  if (Number.isNaN(ts)) return null
  const c = countdown(ts - skew, nowMs)
  return { text: c.text, overdue: c.past }
}


// UpdateInfo is what /v1/update/check returns.
interface UpdateInfo {
  current_version: string
  latest_version: string
  update_available: boolean
  release_url?: string
  release_name?: string
  published_at?: string
  error?: string
}

// UpgradeProgressEvent is one SSE event from /v1/update/run.
interface UpgradeProgressEvent {
  stage: string
  percent?: number
  message?: string
  version?: string
}

// authGate is the one place that decides whether the browser holds a valid
// credential. It hits a PROTECTED endpoint: /v1/health is public, so it would
// always return true. /v1/sessions requires the cookie and returns 401 without
// one, which is the actual signal we need.
async function authGate(): Promise<boolean> {
  try {
    const res = await fetch('/v1/sessions', { credentials: 'same-origin' })
    return res.ok
  } catch {
    return false
  }
}

// clearCredential asks the gateway to delete the cookie and also clears any
// local storage the app keeps, so nothing survives a rejected token.
async function clearCredential(): Promise<void> {
  try {
    await fetch('/v1/webui/session', { method: 'DELETE', credentials: 'same-origin' })
  } catch { /* ignore */ }
  try { localStorage.removeItem(STORAGE_KEY) } catch { /* ignore */ }
}

// submitToken trades a token (or password) the user typed for the browser's
// cookie. Returns true on success, false on rejection.
async function submitToken(token: string): Promise<boolean> {
  try {
    const res = await fetch('/v1/webui/session', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { Authorization: 'Bearer ' + token }
    })
    return res.ok
  } catch {
    return false
  }
}

// api is the one place a request is built. The cookie is sent automatically
// by the browser: nothing here sets a header.
//
// On 401, the credential is stale: the cookie is cleared and an event is
// dispatched so the blocking auth modal reappears.
async function api(path: string, options?: RequestInit): Promise<Response> {
  const res = await fetch(path, { credentials: 'same-origin', ...options })
  if (res.status === 401) {
    await clearCredential()
    window.dispatchEvent(new CustomEvent('motita:auth-needed'))
    throw new Error('unauthorised')
  }
  return res
}

// classifyProgress reads the prefix of a progress line and returns the kind.
function classifyProgress(text: string): string | null {
  if (/^running:/.test(text)) return 'command'
  if (/^action:/.test(text)) return 'reasoning'
  if (/^(validating|validation)/.test(text)) return 'check'
  if (/^(planning|plan ready)/.test(text)) return 'plan'
  if (/^(analysing|understood)/.test(text)) return 'analysis'
  if (/^deciding/.test(text)) return 'reasoning'
  if (/^(task complete|synthesizing)/.test(text)) return 'synthesis'
  return null
}

// parseFrame reads one SSE frame (text between two blank lines).
function parseFrame(raw: string): { id: string | null; event: string | null; data: string | null } {
  const lines = raw.split('\n')
  let id: string | null = null
  let event: string | null = null
  let data: string | null = null
  for (const line of lines) {
    if (line.startsWith('id: ')) id = line.slice(4).trim()
    else if (line.startsWith('event: ')) event = line.slice(7).trim()
    else if (line.startsWith('data: ')) data = line.slice(6)
  }
  return { id, event, data }
}

// modelOptions is the list the Model select offers for a DRAFT provider, built from
// the two catalogues at hand:
//
//   - `live` is what /model-list published, and it describes the SAVED provider, not
//     the draft. It is used only while the draft still points there, because a live
//     list of another provider's models is worse than no list at all.
//   - `all` is /providers, which carries every provider's own catalogue, so a draft
//     provider that has not been saved yet still gets a usable menu.
//
// The draft's current value is appended when neither source knows it, so a model in
// use is never silently dropped from the menu it is being displayed in.
function modelOptions(draftProvider: string, draftModel: string, savedProvider: string, live: string[], all: ProviderInfo[]): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  const add = (m: string) => {
    if (!m || seen.has(m)) return
    seen.add(m)
    out.push(m)
  }
  if (draftProvider === savedProvider) live.forEach(add)
  ;(all.find(p => p.id === draftProvider)?.models || []).forEach(add)
  add(draftModel)
  return out
}

// firstModelOf is the model a provider switch lands on: the first of the catalogue
// the provider published. It exists so a switch never leaves the model empty, which
// the server would read as "leave unchanged" and would therefore keep a model the new
// provider may not serve.
function firstModelOf(provider: string, all: ProviderInfo[]): string {
  return all.find(p => p.id === provider)?.models?.[0] || ''
}

export default function App() {
  const [messages, setMessages] = useState<Message[]>([])
  const [stateText, setStateText] = useState('connecting')
  const [stateBad, setStateBad] = useState(false)
  const [running, setRunning] = useState(false)
  const [activity, setActivity] = useState<string | null>(null)
  const [activityKind, setActivityKind] = useState<string>('')
  const [approval, setApproval] = useState<PendingApproval | null>(null)
  const [input, setInput] = useState('')

  // Session management state.
  const [sessionId, setSessionId] = useState<string>('')
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [projects, setProjects] = useState<ProjectInfo[]>([])
  const [sidebarOpen, setSidebarOpen] = useState<boolean>(() => {
    try { return localStorage.getItem(SIDEBAR_KEY) !== 'false' } catch { return true }
  })
  const [renamingId, setRenamingId] = useState<string | null>(null)
  const [renameValue, setRenameValue] = useState('')
  const [showNewProject, setShowNewProject] = useState(false)
  const [newProjectTitle, setNewProjectTitle] = useState('')
  const [newProjectDesc, setNewProjectDesc] = useState('')
  const [newProjectDir, setNewProjectDir] = useState('')
  const [newProjectGit, setNewProjectGit] = useState('')
  const [creatingProject, setCreatingProject] = useState(false)
  // Toast notification: auto-dismissing message shown at the bottom of the screen.
  // `detail` is the second line a user reads — the human explanation.
  // `technical` is the raw error text, shown small and collapsed behind a
  // "Details" toggle: it names the file that conflicted, which is the one thing
  // a user needs in order to act, without putting git's own words in their face.
  const [toast, setToast] = useState<{ message: string; type: 'success' | 'error'; detail?: string; technical?: string } | null>(null)
  const [toastDetailsOpen, setToastDetailsOpen] = useState(false)
  const [config, setConfig] = useState<ConfigView | null>(null)
  const [showModelSwitcher, setShowModelSwitcher] = useState(false)
  // draftProvider/draftModel are the Model Switcher's working copy. The modal
  // edits THESE, never the saved configuration: a select the user is merely
  // exploring must not change the running session, so nothing is sent to the
  // gateway until Done is pressed. The draft is re-seeded from the saved
  // configuration every time the modal opens, and closing without Done drops it.
  const [draftProvider, setDraftProvider] = useState('')
  const [draftModel, setDraftModel] = useState('')
  const [savingConfig, setSavingConfig] = useState(false)
  const [providers, setProviders] = useState<ProviderInfo[]>([])
  const [modelList, setModelList] = useState<string[]>([])
  const [fetchingModels, setFetchingModels] = useState(false)
  const [showSkillLibrary, setShowSkillLibrary] = useState(false)
  const [showScheduledTasks, setShowScheduledTasks] = useState(false)
  const [scheduledTasks, setScheduledTasks] = useState<ScheduledTask[]>([])
  const [scheduledBusy, setScheduledBusy] = useState(false)
  // nowMs drives the countdown painted inside each task card. It ticks ONLY
  // while the panel is open: a timer that runs (and re-renders the whole app)
  // while nobody is looking at a countdown is pure cost. One second is the
  // unit the countdown itself shows at its finest.
  const [nowMs, setNowMs] = useState(() => Date.now())
  useEffect(() => {
    if (!showScheduledTasks) return
    setNowMs(Date.now())
    const id = setInterval(() => setNowMs(Date.now()), 1000)
    return () => clearInterval(id)
  }, [showScheduledTasks])
  // The create form's working copy. Like the model switcher's draft, it is edited
  // HERE and sent only when the user presses Create: a half-typed cadence must not
  // reach the gateway.
  const [newTaskTitle, setNewTaskTitle] = useState('')
  const [newTaskText, setNewTaskText] = useState('')
  const [newTaskEvery, setNewTaskEvery] = useState('24h')
  const [newTaskKind, setNewTaskKind] = useState<'task' | 'plan'>('task')
  // Confirm-delete modal: when set, shows a modal asking the user to confirm.
  const [confirmDelete, setConfirmDelete] = useState<{ type: 'session' | 'project'; id: string; title: string } | null>(null)
  // Long-press context menu on mobile: when set, shows a small menu with Edit / Delete.
  const [contextMenu, setContextMenu] = useState<{ type: 'session' | 'project'; id: string; title: string; x: number; y: number } | null>(null)
  // Row dropdown menu: which session/project row has its "⋯" menu open.
  const [rowMenu, setRowMenu] = useState<{ type: 'session' | 'project'; id: string; title: string } | null>(null)
  // Closing state: the menu keeps rendering for the length of its exit
  // animation. Without it the element would unmount on the same frame the
  // user clicked outside and the exit animation would never be seen.
  const [rowMenuClosing, setRowMenuClosing] = useState(false)
  const rowMenuTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const rowMenuClosingRef = useRef(false)

  // closeRowMenu plays the exit animation, then unmounts. Idempotent: a
  // second call while the timer runs would otherwise stack timers, and the
  // last one to fire would reset the state under a menu already reopened.
  const closeRowMenu = useCallback(() => {
    if (rowMenuClosingRef.current) return
    rowMenuClosingRef.current = true
    setRowMenuClosing(true)
    if (rowMenuTimer.current) clearTimeout(rowMenuTimer.current)
    rowMenuTimer.current = setTimeout(() => {
      setRowMenu(null)
      setRowMenuClosing(false)
      rowMenuClosingRef.current = false
      rowMenuTimer.current = null
    }, 140)
  }, [])

  // openRowMenu opens the menu, cancelling a close that is mid-flight so
  // the new menu does not inherit the old one's exit state.
  const openRowMenu = useCallback((m: { type: 'session' | 'project'; id: string; title: string } | null) => {
    if (rowMenuTimer.current) { clearTimeout(rowMenuTimer.current); rowMenuTimer.current = null }
    rowMenuClosingRef.current = false
    setRowMenuClosing(false)
    setRowMenu(m)
  }, [])
  // Collapsed projects: a set of project IDs whose session list is hidden.
  // Persisted in localStorage so a user's choice survives a reload, like the
  // sidebar's own open/closed state.
  const [collapsedProjects, setCollapsedProjects] = useState<Set<string>>(() => {
    try {
      const raw = localStorage.getItem(PROJECT_COLLAPSE_KEY)
      if (raw) return new Set(JSON.parse(raw) as string[])
    } catch { /* ignore */ }
    return new Set()
  })
  const toggleProject = (pid: string) => {
    setCollapsedProjects(prev => {
      const next = new Set(prev)
      if (next.has(pid)) next.delete(pid)
      else next.add(pid)
      try { localStorage.setItem(PROJECT_COLLAPSE_KEY, JSON.stringify([...next])) } catch { /* ignore */ }
      return next
    })
  }
  // Slash commands loaded from the backend, and the autocomplete popup state.
  const [slashCommands, setSlashCommands] = useState<{ name: string; aliases: string[]; help: string; arg: string; group: string }[]>([])
  const [slashPopup, setSlashPopup] = useState<{ items: { name: string; aliases: string[]; help: string; arg: string; group: string }[]; index: number } | null>(null)
  // Active slash command tags: when set, the composer shows them as pills
  // above the textarea. Multiple tags can be active at once.
  const [activeTags, setActiveTags] = useState<{ name: string; group: string }[]>([])
  // Auth gate: 'checking' while the initial probe is in flight, 'needed' when
  // the browser has no valid credential, 'ok' when it does. The blocking modal
  // is rendered whenever authState !== 'ok'.
  const [authState, setAuthState] = useState<'checking' | 'needed' | 'ok'>('checking')
  const [authInput, setAuthInput] = useState('')
  const [authError, setAuthError] = useState('')
  const [authBusy, setAuthBusy] = useState(false)

  // Update / upgrade state.
  const [updateInfo, setUpdateInfo] = useState<UpdateInfo | null>(null)
  const [showUpgrade, setShowUpgrade] = useState(false)
  const [upgradeProgress, setUpgradeProgress] = useState<UpgradeProgressEvent | null>(null)
  const [upgradeBusy, setUpgradeBusy] = useState(false)
  const [upgradeError, setUpgradeError] = useState('')
  // Whether the raw upgrade error text is unfolded. Collapsed by default, for
  // the same reason as the toast: the explanation is the point, the exception
  // string is a footnote for a bug report.
  const [showUpgradeError, setShowUpgradeError] = useState(false)

  // plan and task are mutually exclusive — if one is already an active tag,
  // the other is blocked from being added.
  const isCommandBlocked = (cmdName: string) => {
    if (cmdName === '/plan') return activeTags.some(t => t.name === '/task')
    if (cmdName === '/task') return activeTags.some(t => t.name === '/plan')
    return false
  }

  const lastIdRef = useRef(0)
  const runningRef = useRef(false)
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  const msgIdRef = useRef(0)
  const sessionRef = useRef('')

  const nextId = () => ++msgIdRef.current

  // Auto-scroll on new messages or activity.
  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: 'smooth' })
  }, [messages, activity])

  // Persist sidebar preference.
  useEffect(() => {
    try { localStorage.setItem(SIDEBAR_KEY, String(sidebarOpen)) } catch { /* ignore */ }
  }, [sidebarOpen])

  // Auto-dismiss toast. One timer, one place: inline setTimeout calls used to
  // race each other and a stale timer could close a newer toast early.
  // Durations: an error needs reading, so it stays longest; the "new version"
  // toast is clickable and important, so it stays longer than a plain success.
  useEffect(() => {
    setToastDetailsOpen(false)
    if (!toast) return
    const isUpdate = toast.message.includes('New version')
    const ms = toast.type === 'error' ? 12000 : isUpdate ? 8000 : 5000
    const t = setTimeout(() => setToast(null), ms)
    return () => clearTimeout(t)
  }, [toast])

  // runSessionRef is the conversation the run in flight was started in. A run
  // belongs to its SESSION, not to the tab, and the user is free to walk away
  // from it: without pinning the id, an answer arriving after a session switch
  // would clear the flag on whichever conversation happened to be on screen -
  // leaving the one that actually finished looking busy and the one that is
  // idle looking busy too.
  const runSessionRef = useRef('')

  // answeredRef is the conversation whose answer THIS TAB has already seen but
  // whose run the gateway still reports as in flight.
  //
  // The gateway is telling the TRUTH in that window: the turn's goroutine has
  // not returned yet. It generates the auto-title and saves the session BEFORE
  // its deferred releaseRunSlot, and that ordering is deliberate - the gateway's
  // own end-to-end test depends on the slot still being held at that point. So
  // a list read taken in that window really does say a run is in flight, and
  // the refresh that finish() issues (to pick up the new title) is exactly such
  // a read: it turned the spinner back ON over a conversation that had already
  // answered, for as long as the title took to generate.
  //
  // The row is masked for that session until the gateway's OWN flag goes false,
  // which is why this is not a timer: the release happens at a moment only the
  // gateway knows, and a fixed expiry would either un-mask too early (the
  // spinner flashes back) or too late. A new run cannot start in that
  // conversation before the release - the slot is what refuses it - so masking
  // until then can never hide real work.
  //
  // The mask is applied when RENDERING, never to the stored list: the gateway's
  // answer is kept verbatim so `anyRunning` below still counts it, and the poll
  // therefore keeps running exactly long enough to observe the release and
  // clear this.
  const answeredRef = useRef('')
  // answeredUntil is the backstop for a release that is never observed (a lost
  // fetch, a gateway that went away). Comfortably past the title generator's
  // own 15s ceiling, so it can only fire when something has genuinely gone
  // wrong - and then it un-masks rather than leaving a row dark forever.
  const answeredUntil = useRef(0)
  const answeredHere = (id: string) => answeredRef.current === id && Date.now() < answeredUntil.current

  const setRunningState = (r: boolean, sessionFor = sessionRef.current) => {
    runningRef.current = r
    setRunning(r)
    // Fold this tab's own run into the SESSION LIST, so the row that owns it
    // paints its spinner from the same field as every other row.
    //
    // Both directions matter, and the list alone cannot carry either one in
    // time:
    //
    //   * true, at the instant the user sends - the gateway has not answered a
    //     single /v1/sessions yet, and polling would leave the spinner off for
    //     up to a tick while a turn is visibly running. It also clears any
    //     latch, because a new turn supersedes the old answer.
    //   * false, the moment the answer arrives, which LATCHES the session so
    //     the refresh finish() issues cannot light it back up.
    //
    // setSessions is used and not fetchSessions: the local answer is
    // authoritative in both directions, and a request issued here would be the
    // stale one it exists to correct.
    setSessions(prev => prev.map(s => s.id === sessionFor ? { ...s, running: r } : s))
    if (r) {
      answeredRef.current = ''
      answeredUntil.current = 0
    } else {
      answeredRef.current = sessionFor
      answeredUntil.current = Date.now() + 30000
    }
  }

  const setState = (text: string, bad = false) => {
    setStateText(text)
    setStateBad(bad)
  }

  // exchange trades the URL fragment for the cookie.
  // The fragment is never sent to the server; once traded, it is erased.
  const exchange = useCallback(async (): Promise<boolean> => {
    const hash = window.location.hash || ''
    const marker = hash.indexOf('t=')
    if (marker === -1) return false
    const token = hash.slice(marker + 2)
    if (!token) return false
    try {
      const res = await fetch('/v1/webui/session', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { Authorization: 'Bearer ' + token }
      })
      // Erased whatever the answer: a token that did not work is not one to keep.
      history.replaceState(null, '', window.location.pathname + window.location.search)
      return res.ok
    } catch {
      history.replaceState(null, '', window.location.pathname + window.location.search)
      return false
    }
  }, [])

  // fetchSessions loads the list of conversations from the gateway.
  const fetchSessions = useCallback(async (): Promise<SessionInfo[]> => {
    try {
      const res = await api('/v1/sessions')
      const data = await res.json()
      const list: SessionInfo[] = data.sessions || []
      setSessions(list)
      // The gateway has now been asked, and it has answered. If it says the
      // conversation whose answer we already have is NOT running, the run slot
      // has been released - so the latch that was masking it has done its job
      // and is cleared HERE, on the gateway's own word, rather than on a timer.
      // (A stale read in the window is harmless by construction: it leaves the
      // latch exactly as it was.)
      if (answeredRef.current && !list.some(s => s.id === answeredRef.current && s.running)) {
        answeredRef.current = ''
        answeredUntil.current = 0
      }
      return list
    } catch {
      return []
    }
  }, [])

  // Poll session list while a run is in flight, so the sidebar shows a
  // spinner on whichever conversations the agent is working in.
  //
  // Two rules, and the earlier version got both wrong:
  //
  //  1. It polls while ANY conversation is running, not only the one this tab
  //     started. A run belongs to the SESSION, not to a connection: another
  //     client, a scheduled task, or a session resumed after a gateway restart
  //     can be the one working, and a list this tab never refreshed would show
  //     no sign of it.
  //
  //  2. It does NOT stop the instant this tab's own run ends. The gateway
  //     appends the `done` event and finishes the run BEFORE its deferred
  //     releaseRunSlot runs, so a list read taken right after the answer can
  //     still report `running: true`. That read used to be the last one - the
  //     interval was torn down in the same render, because it was keyed on the
  //     local run alone - and the result was a spinner left turning on a
  //     conversation that had already answered. Keying the interval on "is
  //     anything running" makes that final, stale read harmless: it is no
  //     longer the last one, and the next tick clears the flag the gateway has
  //     by then released.
  const anyRunning = sessions.some(s => s.running)
  useEffect(() => {
    if (!running && !anyRunning) return
    const interval = setInterval(() => fetchSessions(), 3000)
    return () => clearInterval(interval)
  }, [running, anyRunning, fetchSessions])

  // fetchProjects loads the list of projects from the gateway.
  const fetchProjects = useCallback(async (): Promise<ProjectInfo[]> => {
    try {
      const res = await api('/v1/projects')
      const data = await res.json()
      const list: ProjectInfo[] = data.projects || []
      setProjects(list)
      return list
    } catch {
      return []
    }
  }, [])

  // createProject sends a new project to the gateway. When a git URL is set,
  // the gateway clones the repo and returns the clone log, which we show in
  // the modal so the user can see what happened.
  const createProject = useCallback(async () => {
    const title = newProjectTitle.trim()
    let dir = newProjectDir.trim()
    const gitUrl = newProjectGit.trim()
    // Auto-derive folder name from git URL if not provided.
    if (!dir && gitUrl) {
      // Extract repo name from URL: git@github.com:user/repo.git -> repo
      // https://github.com/user/repo.git -> repo
      const match = gitUrl.match(/(?:\/|:)[^/]+\/([^/]+?)(?:\.git)?$/)
      dir = match ? match[1] : ''
    }
    if (!title || !dir) return
    setCreatingProject(true)
    try {
      const res = await api('/v1/projects', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          title,
          description: newProjectDesc.trim(),
          dir,
          git_url: gitUrl || undefined,
        })
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        setToast({ message: err.error || 'could not create the project', type: 'error' })
        setState(err.error || 'could not create the project', true)
        setCreatingProject(false)
        return
      }
      const data = await res.json()
      await fetchProjects()
      // Auto-create a session in the new project so the user lands in the chat.
      if (data.id) {
        setTimeout(() => {
          const evt = new CustomEvent('motita:create-session', { detail: { project_id: data.id } })
          window.dispatchEvent(evt)
        }, 100)
      }
      // Show a toast and close the modal.
      setToast({ message: `Project "${title}" created` + (data.clone_log ? ' and repo cloned' : ''), type: 'success' })
      setShowNewProject(false)
      setNewProjectTitle('')
      setNewProjectDesc('')
      setNewProjectDir('')
      setNewProjectGit('')
      setCreatingProject(false)
    } catch (e) {
      setToast({ message: 'Could not create the project: ' + String(e), type: 'error' })
      setState('could not create the project', true)
      setCreatingProject(false)
    }
  }, [newProjectTitle, newProjectDesc, newProjectDir, newProjectGit, fetchProjects])

  // deleteProject removes a project from the gateway.
  const deleteProject = useCallback(async (id: string) => {
    try {
      const res = await api('/v1/projects/' + id, { method: 'DELETE' })
      if (!res.ok && res.status !== 204) return
      await fetchProjects()
    } catch { /* ignore */ }
  }, [fetchProjects])

  // loadSchedules reads the tasks the gateway holds. An empty LIST rather than an
  // error is what the gateway answers when scheduling is off, so this cannot fail
  // for that reason and does not need a branch for it.
  const loadSchedules = useCallback(async () => {
    try {
      const res = await api('/v1/schedules')
      if (!res.ok) return
      const data = await res.json()
      setScheduledTasks(Array.isArray(data.schedules) ? data.schedules : [])
    } catch { /* the toast on a real failure is the caller's job */ }
  }, [])

  const createSchedule = async () => {
    if (!newTaskTitle.trim() || !newTaskText.trim()) return
    setScheduledBusy(true)
    try {
      const res = await api('/v1/schedules', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          title: newTaskTitle.trim(),
          task: newTaskText.trim(),
          kind: newTaskKind,
          every: newTaskEvery.trim(),
        }),
      })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        setToast({ message: 'The task could not be created', type: 'error', detail: body.error })
        return
      }
      setNewTaskTitle(''); setNewTaskText('')
      await loadSchedules()
    } finally { setScheduledBusy(false) }
  }

  const toggleSchedule = async (t: ScheduledTask) => {
    const res = await api('/v1/schedules/' + t.id, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: !t.enabled }),
    })
    if (res.ok) await loadSchedules()
  }

  const runScheduleNow = async (t: ScheduledTask) => {
    const res = await api('/v1/schedules/' + t.id + '/run', { method: 'POST' })
    if (res.status === 409) {
      const body = await res.json().catch(() => ({}))
      setToast({ message: 'That task could not start', type: 'error', detail: body.error })
      return
    }
    if (res.ok) {
      setToast({ message: 'Task started', type: 'success', detail: 'It is running in ' + t.session_id + '.' })
    }
  }

  const deleteSchedule = async (t: ScheduledTask) => {
    const res = await api('/v1/schedules/' + t.id, { method: 'DELETE' })
    if (res.ok) await loadSchedules()
  }

  // The countdown is drawn between the BROWSER's clock and next_run, which the
  // GATEWAY computed. When the two machines disagree the countdown lies by the
  // difference ("4h" on a task that fires in a minute), and the gateway is the
  // one holding the schedule store: this is the smallest correction that keeps
  // the number honest. It is a memo and not state because loadSchedules is not
  // in the render path - a skew from a set of tasks a second old is fine, and
  // this way there is no second render.
  const clockSkew = useMemo(() => {
    let min = nextClockSkew
    for (const t of scheduledTasks) {
      const ts = Date.parse(t.next_run)
      if (Number.isNaN(ts)) continue
      min = Math.min(min, Date.now() - (ts - durationOf(t.every).ms))
    }
    return min
  }, [scheduledTasks])

  // switchSession loads the transcript for a given session id and adopts it.
  const switchSession = useCallback(async (id: string) => {
    setSessionId(id)
    sessionRef.current = id
    try { localStorage.setItem(STORAGE_KEY, id) } catch { /* ignore */ }
    setMessages([])
    setActivity(null)
    setApproval(null)
    lastIdRef.current = 0
    // This tab's run belongs to ONE conversation, and it keeps running while
    // the user looks elsewhere. Carry its flag into the row it belongs to, so
    // coming back to that conversation shows the spinner its own turn earned -
    // the gateway does not know about this tab's view of the list, and the
    // next fetchSessions would otherwise paint the row idle for a tick.
    const liveRun = runSessionRef.current
    if (liveRun) setRunningState(true, liveRun)
    // Load transcript for the new session.
    try {
      const res = await api('/v1/sessions/' + id + '/messages')
      const data = await res.json()
      const msgs = data.messages || []
      const out: Message[] = []
      for (const m of msgs) {
        if (m.User) out.push({ id: nextId(), role: 'user', text: m.User })
        if (m.Agent) out.push({ id: nextId(), role: 'agent', text: m.Agent })
      }
      if (out.length === 0) {
        out.push({ id: nextId(), role: 'agent', text: 'Nothing yet. Ask for something below.', kind: 'kind' })
      }
      setMessages(out)
      setState('ready')
      // Load config so the header shows the current provider/model.
      try {
        const cfgRes = await api('/v1/sessions/' + id + '/config')
        const cfgData: ConfigView = await cfgRes.json()
        setConfig(cfgData)
      } catch { /* non-fatal */ }
    } catch {
      setState('could not load the conversation', true)
    }
  }, [])

  // createSession opens a new conversation and switches to it.
  const createSession = useCallback(async (projectId?: string) => {
    try {
      const body = projectId ? JSON.stringify({ project_id: projectId }) : '{}'
      const res = await api('/v1/sessions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body,
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        setState(err.error || 'could not create a session', true)
        return
      }
      const data: SessionInfo = await res.json()
      await fetchSessions()
      await switchSession(data.id)
    } catch {
      setState('could not create a session', true)
    }
  }, [fetchSessions, switchSession])

  // Listen for the custom event from createProject to auto-create a session.
  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent).detail
      if (detail && detail.project_id) {
        createSession(detail.project_id)
      }
    }
    window.addEventListener('motita:create-session', handler)
    return () => window.removeEventListener('motita:create-session', handler)
  }, [createSession])

  // deleteSession removes a conversation from the gateway and the sidebar.
  const deleteSession = useCallback(async (id: string) => {
    try {
      const res = await api('/v1/sessions/' + id, { method: 'DELETE' })
      // 204 = deleted, 200 = default session was reset instead of removed.
      if (!res.ok && res.status !== 204 && res.status !== 200) {
        const err = await res.json().catch(() => ({}))
        setState(err.error || 'could not delete the session', true)
        return
      }
      // Remove the deleted session from the local state IMMEDIATELY, before
      // the refetch. A 3-second polling interval (or a run's deferred
      // saveSession resurrecting the file) can slip a stale list in between
      // the DELETE and fetchSessions, making the session flicker back into
      // the sidebar. For 200 (default reset) we keep the row but clear its
      // title locally so the placeholder shows without waiting for the refetch.
      if (res.status === 204) {
        setSessions(prev => prev.filter(s => s.id !== id))
      } else {
        setSessions(prev => prev.map(s => s.id === id ? { ...s, title: 'New session' } : s))
      }
      const list = await fetchSessions()
      // If we deleted the active session, switch to the most recent remaining one.
      if (id === sessionRef.current) {
        if (res.status === 200) {
          // Default session was reset — reload its now-empty transcript.
          await switchSession('default')
        } else {
          const last = list.find(s => s.id !== id)
          if (last) {
            await switchSession(last.id)
          } else {
            // No sessions left — go back to default.
            await switchSession('default')
          }
        }
      }
    } catch {
      setState('could not delete the session', true)
    }
  }, [fetchSessions, switchSession])

  // renameSession changes the title of a conversation.
  const renameSession = useCallback(async (id: string, title: string) => {
    const trimmed = title.trim()
    if (!trimmed) return
    try {
      const res = await api('/v1/sessions/' + id, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: trimmed })
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        setState(err.error || 'could not rename the session', true)
        return
      }
      const updated: SessionInfo = await res.json()
      setSessions(prev => prev.map(s => s.id === updated.id ? updated : s))
    } catch {
      setState('could not rename the session', true)
    }
    setRenamingId(null)
  }, [])

  // fetchProviders loads the available providers for the current session.
  const fetchProviders = useCallback(async () => {
    if (!sessionId) return
    try {
      const res = await api('/v1/sessions/' + sessionId + '/providers')
      const data = await res.json()
      setProviders(data.providers || [])
    } catch {
      // non-fatal: the modal opens with an empty list
    }
  }, [sessionId])

  // fetchModelList fetches the live model list from the provider API.
  //
  // It lists the models of the provider the GATEWAY is configured with, which is
  // the saved one and not the draft: the endpoint takes no provider argument. That
  // is why the modal only shows this list while the draft still points at the saved
  // provider, and falls back to the catalogue otherwise.
  const fetchModelList = useCallback(async () => {
    if (!sessionId) return
    setFetchingModels(true)
    setModelList([])
    try {
      const res = await api('/v1/sessions/' + sessionId + '/model-list')
      if (res.ok) {
        const data = await res.json()
        setModelList(data.models || [])
      }
    } catch {
      // non-fatal: the select shows the static catalog instead
    }
    setFetchingModels(false)
  }, [sessionId])

  // openModelSwitcher shows the modal with a draft seeded from the SAVED
  // configuration. Seeding on open, rather than on every change, is what makes
  // Done the only commit point: an edit that was cancelled and a modal that was
  // dismissed both leave the session's provider and model untouched.
  const openModelSwitcher = useCallback(() => {
    setDraftProvider(config?.provider || '')
    setDraftModel(config?.model || '')
    fetchProviders()
    fetchModelList()
    setShowModelSwitcher(true)
  }, [config, fetchProviders, fetchModelList])

  // closeModelSwitcher dismisses the modal and throws the draft away. The saved
  // configuration is what the next open seeds from, so nothing has to be undone.
  //
  // Nothing is sent here even when the draft differs: Done is the only commit
  // point, and a dismissal is not one. A user who picks a model and then changes
  // their mind must be able to walk away from the choice.
  const closeModelSwitcher = useCallback(() => {
    setShowModelSwitcher(false)
  }, [])

  // discardModelSwitcher is the Close (✕) button: it drops the draft and reopens
  // the modal on what the session is actually running, so the screen cannot be
  // left showing a selection that was never applied.
  const discardModelSwitcher = useCallback(() => {
    setDraftProvider(config?.provider || '')
    setDraftModel(config?.model || '')
    setShowModelSwitcher(false)
  }, [config])

  // applyProviderModel is what Done does, and it is the ONE call that reaches the
  // gateway. Both fields travel together: the server treats an empty model as
  // "leave unchanged", so a provider switch sent alone would keep a model that the
  // new provider may not serve.
  const applyProviderModel = useCallback(async () => {
    if (!sessionId) return
    setSavingConfig(true)
    try {
      const res = await api('/v1/sessions/' + sessionId + '/config', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ provider: draftProvider, model: draftModel })
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        setState(err.error || 'could not update the configuration', true)
        // The modal stays open on a failure: the choice is still on screen and
        // can be retried, and a closed modal would read as if it had worked.
        return
      }
      const updated: ConfigView = await res.json()
      setConfig(updated)
      setShowModelSwitcher(false)
    } catch {
      setState('could not update the configuration', true)
    } finally {
      // In a finally, not at the end of the try: the failure path returns early,
      // and a flag left set would leave Done disabled and the spinner running
      // with no way back.
      setSavingConfig(false)
    }
  }, [sessionId, draftProvider, draftModel])

  // checkForUpdates polls /v1/update/check. When a new version is found, a
  // toast is shown (unless the user has dismissed this version before) and
  // updateInfo is set so the sidebar can show the "Upgrade Motita" button.
  const checkForUpdates = useCallback(async () => {
    try {
      const res = await api('/v1/update/check')
      if (!res.ok) return
      const data: UpdateInfo = await res.json()
      setUpdateInfo(data)
      if (data.update_available && data.latest_version) {
        // Check if the user has dismissed this version.
        try {
          const dismissed = localStorage.getItem(UPGRADE_DISMISS_KEY)
          if (dismissed !== data.latest_version) {
            setToast({ message: `New version ${data.latest_version} available — click to upgrade`, type: 'success' })
          }
        } catch { /* ignore */ }
      }
    } catch { /* non-fatal */ }
  }, [])

  // runUpgrade starts the upgrade stream and updates the progress bar.
  const runUpgrade = useCallback(async () => {
    setUpgradeBusy(true)
    setUpgradeError('')
    setShowUpgradeError(false)
    setUpgradeProgress({ stage: 'starting', percent: 0, message: 'Starting upgrade…' })
    try {
      const res = await api('/v1/update/run', { method: 'POST' })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        setUpgradeError(err.error || 'could not start the upgrade')
        setUpgradeBusy(false)
        return
      }
      const reader = res.body!.getReader()
      const decoder = new TextDecoder()
      let buffer = ''
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buffer += decoder.decode(value, { stream: true })
        let idx: number
        while ((idx = buffer.indexOf('\n\n')) !== -1) {
          const frame = buffer.slice(0, idx)
          buffer = buffer.slice(idx + 2)
          // Parse SSE: find data: line
          let dataLine = ''
          for (const line of frame.split('\n')) {
            if (line.startsWith('data: ')) dataLine = line.slice(6)
          }
          if (dataLine) {
            try {
              const evt: UpgradeProgressEvent = JSON.parse(dataLine)
              setUpgradeProgress(evt)
              if (evt.stage === 'error') {
                setUpgradeError(evt.message || 'upgrade failed')
                setUpgradeBusy(false)
              } else if (evt.stage === 'done') {
                setUpgradeBusy(false)
                // The gateway is restarting. Show a message and wait for it
                // to come back, then reload the page.
                setTimeout(() => {
                  // Poll health until the gateway is back.
                  const poll = setInterval(async () => {
                    try {
                      const h = await fetch('/v1/health')
                      if (h.ok) {
                        clearInterval(poll)
                        // Reload to pick up the new version.
                        window.location.reload()
                      }
                    } catch { /* keep polling */ }
                  }, 1000)
                  // Give up after 30 seconds.
                  setTimeout(() => clearInterval(poll), 30000)
                }, 2000)
              }
            } catch { /* ignore parse error */ }
          }
        }
      }
    } catch (e) {
      setUpgradeError(String(e))
      setUpgradeBusy(false)
    }
  }, [])

  // dismissUpgradeToast hides the "new version" toast and remembers the
  // dismissal so it does not reappear for the same version.
  const dismissUpgradeToast = useCallback(() => {
    if (updateInfo?.latest_version) {
      try { localStorage.setItem(UPGRADE_DISMISS_KEY, updateInfo.latest_version) } catch { /* ignore */ }
    }
    setToast(null)
  }, [updateInfo])

  // transcript paints the conversation that already exists.
  const transcript = useCallback(async () => {
    const res = await api('/v1/sessions/' + sessionRef.current + '/messages')
    const data = await res.json()
    const msgs = data.messages || []
    if (msgs.length === 0) {
      setMessages([{ id: nextId(), role: 'agent', text: 'Nothing yet. Ask for something below.', kind: 'kind' }])
      return
    }
    const out: Message[] = []
    for (const m of msgs) {
      if (m.User) out.push({ id: nextId(), role: 'user', text: m.User })
      if (m.Agent) out.push({ id: nextId(), role: 'agent', text: m.Agent })
    }
    setMessages(out)
  }, [])

  // answerApproval sends the approval response.
  const answerApproval = useCallback(async (id: string, approve: boolean) => {
    setApproval(null)
    try {
      await api('/v1/sessions/' + sessionRef.current + '/runs/approval', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, approve })
      })
    } catch {
      setState('could not answer the approval', true)
    }
  }, [])

  // finish marks the run as done.
  const finish = useCallback(() => {
    // Cleared against the session the run was STARTED in, not against whatever
    // is on screen now: a user who switched away mid-turn must not have the
    // finished run's flag land on the conversation they switched to.
    setRunningState(false, runSessionRef.current || sessionRef.current)
    runSessionRef.current = ''
    setActivity(null)
    setState('ready')
    // After a run finishes, refresh the session list so the auto-title shows up.
    fetchSessions()
  }, [fetchSessions])

  // dispatchEvent handles one parsed SSE event.
  const dispatchEvent = useCallback((name: string, data: string | null, id: string | null) => {
    if (id) lastIdRef.current = parseInt(id, 10) || lastIdRef.current
    if (data === null) return
    let payload: any
    try {
      payload = JSON.parse(data)
    } catch {
      setMessages(prev => [...prev, { id: nextId(), role: 'agent', text: data }])
      return
    }
    switch (name) {
    case 'attached':
      if (payload.dropped > 0) {
        setMessages(prev => [...prev, {
          id: nextId(), role: 'agent', kind: 'kind',
          text: '(' + payload.dropped + ' event(s) were not kept while nothing was listening)'
        }])
      }
      if (payload.pending_approval) setApproval(payload.pending_approval)
      break
    case 'progress':
      setActivity(payload.text || '')
      setActivityKind(classifyProgress(payload.text || '') ?? '')
      break
    case 'approval':
      setActivity(null)
      setApproval(payload)
      break
    case 'done':
      setActivity(null)
      if (payload.result) {
        setMessages(prev => [...prev, { id: nextId(), role: 'agent', text: payload.result }])
      }
      finish()
      break
    case 'error':
      setActivity(null)
      if (payload.error) {
        setMessages(prev => [...prev, { id: nextId(), role: 'agent', text: payload.error, kind: 'error' }])
      }
      finish()
      break
    // Unknown events are ignored: a future server may add one.
    }
  }, [finish])

  // readStream consumes an SSE response body and dispatches each frame.
  const readStream = useCallback(async (res: Response) => {
    const reader = res.body!.getReader()
    const decoder = new TextDecoder()
    let buffer = ''
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      let idx: number
      while ((idx = buffer.indexOf('\n\n')) !== -1) {
        const frame = parseFrame(buffer.slice(0, idx))
        buffer = buffer.slice(idx + 2)
        if (frame.data !== null) dispatchEvent(frame.event || 'message', frame.data, frame.id)
      }
    }
    if (buffer.trim()) {
      const frame = parseFrame(buffer)
      if (frame.data !== null) dispatchEvent(frame.event || 'message', frame.data, frame.id)
    }
  }, [dispatchEvent])

  // followReconnect reattaches to a dropped stream.
  const followReconnect = useCallback(async () => {
    let res: Response
    try {
      res = await api('/v1/sessions/' + sessionRef.current + '/events?from=' + lastIdRef.current)
    } catch {
      if (runningRef.current) {
        setState('reconnecting', true)
        reconnectTimer.current = setTimeout(followReconnect, 1000)
      }
      return
    }
    if (res.status === 404) {
      if (runningRef.current) {
        finish()
        await transcript()
      }
      return
    }
    if (!res.ok) {
      if (runningRef.current) {
        setState('reconnecting', true)
        reconnectTimer.current = setTimeout(followReconnect, 1000)
      }
      return
    }
    try {
      await readStream(res)
    } catch {
      // Stream broke mid-read.
    }
    if (runningRef.current) {
      setState('reconnecting', true)
      reconnectTimer.current = setTimeout(followReconnect, 1000)
    }
  }, [finish, transcript, readStream])

  // submit sends a task and reads the SSE response.
  const submit = useCallback(async (text: string) => {
    setMessages(prev => [...prev, { id: nextId(), role: 'user', text }])
    // Pinned BEFORE the run starts, so the spinner and its clearing are both
    // addressed to the conversation this turn belongs to.
    runSessionRef.current = sessionRef.current
    setRunningState(true)
    setState('working')
    let res: Response
    try {
      res = await api('/v1/sessions/' + sessionRef.current + '/task', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ task: text })
      })
    } catch {
      setState('the gateway refused the turn', true)
      finish()
      return
    }
    if (!res.ok) {
      let msg = 'the gateway refused the turn'
      try {
        const err = await res.json()
        if (err.error) msg = err.error
      } catch { /* keep generic */ }
      setState(msg, true)
      finish()
      return
    }
    try {
      await readStream(res)
    } catch {
      // Stream broke mid-flight — try to reconnect.
      if (runningRef.current) {
        setState('reconnecting', true)
        reconnectTimer.current = setTimeout(followReconnect, 1000)
      }
      return
    }
    // Stream ended normally. If finish() was already called by the done/error
    // event, runningRef is false and this is a no-op. If the server closed
    // the connection without a terminal event, we must still finish.
    if (runningRef.current) {
      finish()
    }
  }, [finish, readStream, followReconnect])

  // handleSubmit is called when the form is submitted.
  const handleSubmit = (e: Event) => {
    e.preventDefault()
    const argText = input.trim()
    const tagText = activeTags.map(t => t.name).join(' ')
    const text = activeTags.length > 0 ? (argText ? tagText + ' ' + argText : tagText) : argText
    if (!text || runningRef.current) return
    // Remove placeholder messages (.kind) before the first real turn.
    setMessages(prev => prev.filter(m => m.kind !== 'kind'))
    setInput('')
    setActiveTags([])
    submit(text)
  }

  // handleKeydown: Enter sends on desktop (no modifier), Shift+Enter inserts newline.
  // On touch devices Enter always inserts a newline — a physical keyboard is
  // the signal that single-line Enter-to-send is expected.
  // When the slash command popup is open, ArrowUp/Down navigate and Tab/Enter
  // accept the selection.
  const handleKeydown = (e: KeyboardEvent) => {
    // Slash popup navigation.
    if (slashPopup) {
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        setSlashPopup({ ...slashPopup, index: (slashPopup.index + 1) % slashPopup.items.length })
        return
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault()
        setSlashPopup({ ...slashPopup, index: (slashPopup.index - 1 + slashPopup.items.length) % slashPopup.items.length })
        return
      }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey)) {
        e.preventDefault()
        const selected = slashPopup.items[slashPopup.index]
        if (selected && !isCommandBlocked(selected.name)) {
          setActiveTags(prev => [...prev, { name: selected.name, group: selected.group }])
          setInput('')
          setSlashPopup(null)
        }
        return
      }
      if (e.key === 'Escape') {
        e.preventDefault()
        setSlashPopup(null)
        return
      }
    }
    if (e.key !== 'Enter') return
    // Coarse pointer = touch device; skip Enter-to-send there.
    if (window.matchMedia('(pointer: coarse)').matches) return
    if (e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return
    e.preventDefault()
    const argText = input.trim()
    // If there are active tags, send tags + argument. If no tags and no text, nothing.
    const tagText = activeTags.map(t => t.name).join(' ')
    const text = activeTags.length > 0 ? (argText ? tagText + ' ' + argText : tagText) : argText
    if (!text || runningRef.current) return
    setMessages(prev => prev.filter(m => m.kind !== 'kind'))
    setInput('')
    setActiveTags([])
    submit(text)
  }

  // Start: exchange token, check auth, load sessions, pick up the last session or default.
  useEffect(() => {
    ;(async () => {
      // First, try to exchange the fragment token (from the gateway link).
      try { await exchange() } catch { /* ignore */ }
      // Then check whether the browser now holds a valid credential.
      const ok = await authGate()
      if (!ok) {
        setAuthState('needed')
        return
      }
      setAuthState('ok')
      try {
        await fetchProjects()
        // Load slash commands for autocomplete.
        try {
          const res = await api('/v1/commands')
          const data = await res.json()
          setSlashCommands(data.commands || [])
        } catch { /* non-fatal */ }
        const list = await fetchSessions()
        // Try to resume the last-used session, falling back to default.
        let lastId = ''
        try { lastId = localStorage.getItem(STORAGE_KEY) || '' } catch { /* ignore */ }
        const exists = list.some(s => s.id === lastId)
        const target = exists ? lastId : 'default'
        await switchSession(target)
        setState('ready')
      } catch {
        setState('not connected', true)
      }
    })()
    // Cleanup reconnect timer on unmount.
    return () => {
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current)
    }
  }, [exchange, fetchProjects, fetchSessions, switchSession])

  // Listen for auth-needed events dispatched by api() on 401. The modal
  // reappears and local state is reset so nothing stale is shown.
  useEffect(() => {
    const handler = () => {
      setAuthState('needed')
      setAuthInput('')
      setAuthError('')
      setMessages([])
      setSessions([])
      setProjects([])
      setState('not connected', true)
    }
    window.addEventListener('motita:auth-needed', handler)
    return () => window.removeEventListener('motita:auth-needed', handler)
  }, [])

  // Poll for updates: once on startup (after auth succeeds) and then every
  // 10 minutes. The backend's own hourly check is what populates the cache,
  // so this poll is cheap (it reads the cache, not GitHub).
  useEffect(() => {
    if (authState !== 'ok') return
    checkForUpdates()
    const interval = setInterval(checkForUpdates, 10 * 60 * 1000)
    return () => clearInterval(interval)
  }, [authState, checkForUpdates])

  // submitAuth is called when the user enters a token in the blocking modal.
  const submitAuth = useCallback(async () => {
    const token = authInput.trim()
    if (!token || authBusy) return
    setAuthBusy(true)
    setAuthError('')
    const ok = await submitToken(token)
    if (ok) {
      // Erase the token from the URL fragment if it was there.
      history.replaceState(null, '', window.location.pathname + window.location.search)
      setAuthState('ok')
      setAuthInput('')
      setAuthBusy(false)
      // Now load everything as if the app had just started.
      try {
        await fetchProjects()
        try {
          const res = await api('/v1/commands')
          const data = await res.json()
          setSlashCommands(data.commands || [])
        } catch { /* non-fatal */ }
        const list = await fetchSessions()
        let lastId = ''
        try { lastId = localStorage.getItem(STORAGE_KEY) || '' } catch { /* ignore */ }
        const exists = list.some(s => s.id === lastId)
        const target = exists ? lastId : 'default'
        await switchSession(target)
        setState('ready')
      } catch {
        setState('not connected', true)
      }
    } else {
      setAuthError('That token was rejected. Try again.')
      setAuthBusy(false)
    }
  }, [authInput, authBusy, fetchProjects, fetchSessions, switchSession])

  // Click-outside / Escape closes the row menu, WITH its exit animation.
  // Listening on `document` in the CAPTURE phase (not bubble) so the menu
  // closes even when the click lands on an element that stops propagation —
  // a session row does exactly that, which is why a bubble-phase listener
  // would leave the menu open when the user clicked a different row.
  //
  // The `⋯` toggle itself is excluded: `mousedown` fires BEFORE `click`, so
  // treating it as an outside click would flip the state twice (closed here,
  // then reopened by the button's own handler) and the button would seem dead.
  useEffect(() => {
    if (!rowMenu) return
    const onDown = (e: Event) => {
      const t = e.target as HTMLElement | null
      if (t && (t.closest('.row-menu') || t.closest('button[title="More actions"]'))) return
      closeRowMenu()
    }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') closeRowMenu() }
    document.addEventListener('mousedown', onDown, true)
    document.addEventListener('touchstart', onDown, true)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown, true)
      document.removeEventListener('touchstart', onDown, true)
      document.removeEventListener('keydown', onKey)
    }
  }, [rowMenu, closeRowMenu])

  // Copy button handler: delegate clicks from copy-btn and copy-msg-btn.
  useEffect(() => {
    const handler = (e: Event) => {
      const target = e.target as HTMLElement
      if (target.classList.contains('copy-btn')) {
        const text = target.getAttribute('data-copy-text') || ''
        navigator.clipboard?.writeText(text).then(() => {
          target.textContent = '✓'
          setTimeout(() => { target.textContent = 'copy' }, 1500)
        }).catch(() => {})
      } else if (target.classList.contains('copy-msg-btn')) {
        const text = target.getAttribute('data-raw') || ''
        navigator.clipboard?.writeText(text).then(() => {
          target.textContent = '✓'
          setTimeout(() => { target.textContent = 'copy' }, 1500)
        }).catch(() => {})
      }
    }
    document.addEventListener('click', handler)
    return () => document.removeEventListener('click', handler)
  }, [])

  const stateClass = stateBad ? 'bad' : (stateText === 'working' || stateText === 'reconnecting') ? 'state-working' : ''

  // longPressTimer ref for session/project long-press detection.
  const longPressTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  // startLongPress starts a timer; if it fires before the user moves or lifts,
  // we open the context menu at the touch position.
  const startLongPress = (type: 'session' | 'project', id: string, title: string, e: Event) => {
    const touch = (e as TouchEvent).touches[0]
    const x = touch.clientX
    const y = touch.clientY
    longPressTimer.current = setTimeout(() => {
      setContextMenu({ type, id, title, x, y })
      // Prevent the subsequent click from firing.
      if (longPressTimer.current) { longPressTimer.current = null }
    }, 500)
  }

  const cancelLongPress = () => {
    if (longPressTimer.current) {
      clearTimeout(longPressTimer.current)
      longPressTimer.current = null
    }
  }

  // renderSessionRow draws one session row in the sidebar. Extracted so the
  // project-grouped layout and the free-standing list share the same markup.
  // On desktop, action buttons appear on hover. On mobile, long-press opens
  // a context menu with Edit / Delete.
  const renderSessionRow = (s: SessionInfo) => {
    // `s.running` is the gateway's own answer about this conversation. The
    // local run is folded into the LIST at the two moments this tab knows
    // something the gateway has not said yet (see setRunningState), so a row
    // never has to reason about which client started what - and a run started
    // in another tab, by a scheduled task, or restored after a gateway restart
    // shows up through exactly the same field.
    //
    // `answeredHere` masks the one case where the gateway and the user
    // disagree: the answer has been delivered to this tab but the turn's
    // goroutine has not returned, so the flag is still true for a moment. See
    // answeredRef.
    const busy = s.running && !answeredHere(s.id)
    return (
    <div
      key={s.id}
      class={`session-row group flex items-center gap-2 px-3 py-2 rounded-lg cursor-pointer transition-colors mb-0.5 ${
        s.id === sessionId ? 'bg-accent/10 border border-accent/20' : 'hover:bg-white/5 border border-transparent'
      }`}
      onClick={() => { if (renamingId !== s.id) switchSession(s.id) }}
      onTouchStart={(e) => startLongPress('session', s.id, s.title || s.id, e)}
      onTouchMove={cancelLongPress}
      onTouchEnd={cancelLongPress}
    >
      {/* The activity spinner. It is ALWAYS mounted and absolutely positioned
          against the row (`.session-spinner`, index.css), so it comes and goes
          without moving the title, the timestamp or the facts by a pixel: the
          in-flow version pushed the row's whole content sideways for as long as
          a run lasted. The class carries the state; the transition and the
          rotation are plain CSS, because this build disables Tailwind's
          `animation` plugin and an `animate-spin` icon never turned at all. */}
      <span class={`session-spinner${busy ? ' is-running' : ''}`} aria-hidden="true" />
      {renamingId === s.id ? (
        <>
          <input
            class="flex-1 min-w-0 bg-black/30 border border-accent/30 rounded px-2 py-1 text-sm text-[#e8e8ea] focus:outline-none focus:border-accent"
            value={renameValue}
            autoFocus
            onInput={(e) => setRenameValue((e.target as HTMLInputElement).value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') { renameSession(s.id, renameValue); setRenamingId(null) }
              if (e.key === 'Escape') setRenamingId(null)
            }}
            onClick={(e) => e.stopPropagation()}
          />
          <button
            class="p-1 rounded hover:bg-accent/20 text-accent flex-none"
            title="Confirm"
            onClick={(e) => { e.stopPropagation(); renameSession(s.id, renameValue); setRenamingId(null) }}
          >
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
              <polyline points="20 6 9 17 4 12" />
            </svg>
          </button>
          <button
            class="p-1 rounded hover:bg-white/10 text-[#9a9aaa] flex-none"
            title="Cancel"
            onClick={(e) => { e.stopPropagation(); setRenamingId(null) }}
          >
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
            </svg>
          </button>
        </>
      ) : (
        <>
          {/* Two lines, so the title keeps the full width and the facts sit
              under it instead of competing for the same row: at a narrow
              sidebar the branch used to squeeze the title away. */}
          <div class="flex-1 min-w-0">
            <div class={`truncate text-sm leading-tight ${busy ? 'text-accent' : 'text-[#e8e8ea]'}`}>
              {s.title || s.id}
            </div>
            {/* When it was last used, on its own line. It was sharing the row
                with the branch and the counts, and at a narrow width the three
                wrapped into a paragraph nobody could read: a timestamp and a
                set of labelled facts are two different kinds of information and
                each gets its own line. */}
            <div
              class="mt-0.5 text-[10px] tabular-nums text-muted-foreground opacity-70"
              title={s.last_used ? 'Last used ' + fullUpdated(s.last_used) : undefined}
            >
              {formatUpdated(s.last_used)}
            </div>
            {/* Facts line: every value carries its LABEL and its own colour, so
                a number is never just a number. `main` alone does not say it is
                a branch, and a bare `3` does not say 3 of what. Colour separates
                the kinds at a glance; the label says what each one is on the
                first read, which is what makes colour optional rather than
                load-bearing. */}
            <div class="flex items-center gap-x-1.5 gap-y-1 flex-wrap mt-1 text-[10px]">
              {s.branch && (
                <span
                  class="inline-flex items-center gap-1 flex-none px-1 py-px rounded bg-accent/10"
                  title={'Branch: ' + s.branch}
                >
                  <span class="text-accent/50 uppercase tracking-wide text-[9px]">branch</span>
                  <span class="font-mono text-accent truncate max-w-[7rem]">{shortBranch(s.branch)}</span>
                </span>
              )}
              {sessionWorktreeChip(s) && (
                <span
                  class="inline-flex items-center gap-1 flex-none px-1 py-px rounded bg-[#a0a0f0]/10"
                  title={'Worktree: ' + s.worktree}
                >
                  <span class="text-[#a0a0f0]/50 uppercase tracking-wide text-[9px]">worktree</span>
                  <span class="font-mono text-[#a0a0f0] truncate max-w-[6rem]">{sessionWorktreeChip(s)}</span>
                </span>
              )}
              {!!s.changes && (
                <span
                  class="inline-flex items-center gap-1 flex-none px-1 py-px rounded bg-[#f0a040]/10"
                  title={changesTitle(s.changes)}
                >
                  <span class="text-[#f0a040]/50 uppercase tracking-wide text-[9px]">
                    {s.changes === 1 ? 'change' : 'changes'}
                  </span>
                  <span class="tabular-nums text-[#f0a040]">{s.changes}</span>
                </span>
              )}
            </div>
          </div>
        </>
      )}
      {renamingId !== s.id && (
        <div class={`row-actions relative flex-none${rowMenu?.id === s.id ? ' row-actions-open' : ''}`}>
          <button
            class="p-1 rounded hover:bg-white/10"
            title="More actions"
            onClick={(e) => {
              e.stopPropagation()
              if (rowMenu?.id === s.id) closeRowMenu()
              else openRowMenu({ type: 'session', id: s.id, title: s.title || s.id })
            }}
          >
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <circle cx="12" cy="5" r="1" /><circle cx="12" cy="12" r="1" /><circle cx="12" cy="19" r="1" />
            </svg>
          </button>
          {rowMenu?.id === s.id && (
            <div
              class={`row-menu absolute right-0 top-full mt-1 z-50 frosted rounded-xl border border-white/10 py-1 min-w-[140px]${rowMenuClosing ? ' row-menu-closing' : ''}`}
              onClick={(e) => e.stopPropagation()}
            >
              <button
                class="row-menu-item w-full flex items-center gap-2 px-3 py-2 text-sm text-[#e8e8ea] hover:bg-white/5"
                onClick={(e) => { e.stopPropagation(); setRenamingId(s.id); setRenameValue(s.title || ''); closeRowMenu() }}
              >
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" /><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
                </svg>
                Rename
              </button>
              {s.project_id && (
                <button
                  class={`row-menu-item w-full flex items-center gap-2 px-3 py-2 text-sm ${s.mergeable ? 'text-accent hover:bg-accent/10' : 'text-[#8a8a9a]'}`}
                  disabled={!s.mergeable}
                  title={s.mergeable ? 'Integrate this session\'s work back into the project' : 'Nothing to integrate yet'}
                  onClick={async (e) => {
                    if (!s.mergeable) { e.stopPropagation(); return }
                    e.stopPropagation()
                    closeRowMenu()
                    try {
                      const res = await api('/v1/sessions/' + s.id + '/merge', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: '{}',
                      })
                      if (res.status === 409) {
                        // A conflict is a NORMAL outcome, not a crash: the
                        // session changed lines the project also changed. The
                        // message says what happened, what did not, and what to
                        // do — the raw git text rides along as a muted detail
                        // because it names the file, which is what a user needs
                        // in order to act.
                        const data = await res.json().catch(() => ({}))
                        setToast({
                          type: 'error',
                          message: 'This session conflicts with the project',
                          detail: 'The same lines were changed in both. Nothing was modified — your project is exactly as it was. Bring the project up to date, then try again.',
                          ...(data.error ? { technical: data.error } : {}),
                        })
                      } else if (!res.ok) {
                        const data = await res.json().catch(() => ({}))
                        setToast({
                          type: 'error',
                          message: 'The integration could not be completed',
                          detail: `The gateway answered with an error (${res.status}). Nothing was modified.`,
                          ...(data.error ? { technical: data.error } : {}),
                        })
                      } else {
                        const data = await res.json()
                        setToast({
                          type: 'success',
                          message: 'Changes integrated into the project',
                          detail: `Commit ${data.sha} — ${data.subject}`,
                        })
                      }
                    } catch (err) {
                      setToast({
                        type: 'error',
                        message: 'Could not reach the gateway',
                        detail: 'The integration was not attempted, so nothing changed. Check that the gateway is running and try again.',
                        technical: String(err),
                      })
                    }
                  }}
                >
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M6 3v12" /><circle cx="6" cy="18" r="3" /><path d="M18 21v-12" /><circle cx="18" cy="6" r="3" /><path d="M6 9a9 9 0 0 0 12 6" />
                  </svg>
                  Integrate
                </button>
              )}
              <button
                class="row-menu-item w-full flex items-center gap-2 px-3 py-2 text-sm text-danger hover:bg-danger/10"
                onClick={(e) => { e.stopPropagation(); setConfirmDelete({ type: 'session', id: s.id, title: s.title || s.id }); closeRowMenu() }}
              >
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <polyline points="3 6 5 6 21 6" /><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
                </svg>
                Delete
              </button>
            </div>
          )}
        </div>
      )}
    </div>
    )
  }

  return (
    <div class="app-bg flex h-[100dvh] text-[#e8e8ea] overflow-hidden">
      {/* Blocking auth modal — shown when the browser has no valid credential.
          It covers the entire screen and cannot be dismissed without a token. */}
      {authState !== 'ok' && (
        <div
          class="fixed inset-0 z-[100] bg-black/80 backdrop-blur-sm flex items-center justify-center p-4"
          onClick={(e) => e.stopPropagation()}
        >
          <div
            class="frosted rounded-2xl border border-accent/20 w-full max-w-sm p-6 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div class="flex items-center gap-3 mb-4">
              <div class="w-12 h-12 rounded-full bg-accent/15 flex items-center justify-center flex-none">
                <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent">
                  <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
                  <path d="M7 11V7a5 5 0 0 1 10 0v4" />
                </svg>
              </div>
              <div>
                <h2 class="text-base font-semibold text-[#e8e8ea]">Authentication required</h2>
                <p class="text-xs text-[#9a9aaa] mt-0.5">Enter the gateway token to continue</p>
              </div>
            </div>

            {authState === 'checking' ? (
              <div class="flex items-center justify-center gap-2 py-6 text-sm text-[#9a9aaa]">
                <svg class="animate-spin text-accent" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                </svg>
                <span>Checking credentials…</span>
              </div>
            ) : (
              <form
                onSubmit={(e) => { e.preventDefault(); submitAuth() }}
              >
                <label class="block text-sm text-[#9a9aaa] mb-1.5" htmlFor="auth-token">Token or password</label>
                <input
                  id="auth-token"
                  type="password"
                  class="w-full px-3 py-2.5 rounded-xl bg-black/30 border border-white/10 text-[#e8e8ea] focus:outline-none focus:border-accent font-mono text-sm"
                  value={authInput}
                  onInput={(e) => setAuthInput((e.target as HTMLInputElement).value)}
                  placeholder="Paste the gateway token…"
                  autoFocus
                  autoComplete="off"
                  spellcheck={false}
                  disabled={authBusy}
                />
                {authError && (
                  <p class="text-sm text-danger mt-2 flex items-center gap-1.5">
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                      <circle cx="12" cy="12" r="10" /><line x1="12" y1="8" x2="12" y2="12" /><line x1="12" y1="16" x2="12.01" y2="16" />
                    </svg>
                    {authError}
                  </p>
                )}
                <p class="text-xs text-[#6a6a7a] mt-3 leading-relaxed">
                  Run <code class="font-mono text-accent bg-accent/10 px-1 rounded">motita gateway start</code> in a terminal to print the link, or paste the token here.
                </p>
                <button
                  type="submit"
                  disabled={authBusy || !authInput.trim()}
                  class="w-full min-h-[44px] mt-4 rounded-xl bg-accent text-white font-semibold disabled:opacity-30 disabled:cursor-not-allowed active:scale-95 transition-transform flex items-center justify-center gap-2"
                >
                  {authBusy ? (
                    <>
                      <svg class="animate-spin" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                        <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                      </svg>
                      Connecting…
                    </>
                  ) : 'Connect'}
                </button>
              </form>
            )}
          </div>
        </div>
      )}

      {/* Sidebar — sessions list, toggleable on all sizes. */}
      {sidebarOpen && (
        <>
          {/* Mobile overlay: click to close the sidebar. */}
          <div
            class="fixed inset-0 bg-black/50 z-20 md:hidden"
            onClick={() => setSidebarOpen(false)}
          />
          <aside class="sidebar frosted fixed md:relative inset-y-0 left-0 w-72 z-30 flex flex-col border-r border-white/5">
            {/* Sidebar header */}
            <div class="flex items-center gap-2 px-4 py-3 border-b border-white/5 flex-none">
              <span class="text-accent text-lg">🐱</span>
              <h1 class="text-base font-semibold tracking-wide">Motita</h1>
              {updateInfo?.current_version && (
                <span class="text-[10px] text-[#6a6a7a] font-mono mt-0.5">v{updateInfo.current_version.replace(/^v/, '')}</span>
              )}
              <button
                class="ml-auto p-1.5 rounded-lg hover:bg-white/5 transition-colors"
                onClick={() => setSidebarOpen(false)}
                aria-label="Close sidebar"
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                </svg>
              </button>
            </div>

            {/* New session + New project buttons */}
            <div class="px-3 py-2 flex-none flex gap-2">
              <button
                class="flex-1 flex items-center justify-center gap-2 px-3 py-2.5 rounded-xl bg-accent/10 border border-accent/20 text-accent font-medium hover:bg-accent/20 active:scale-95 transition-all"
                onClick={() => createSession()}
              >
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" />
                </svg>
                New session
              </button>
              <button
                class="flex items-center justify-center px-3 py-2.5 rounded-xl border border-white/10 text-[#e8e8ea] hover:bg-white/5 active:scale-95 transition-all"
                onClick={() => setShowNewProject(true)}
                title="New project"
              >
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z" /><line x1="12" y1="11" x2="12" y2="17" /><line x1="9" y1="14" x2="15" y2="14" />
                </svg>
              </button>
            </div>

            {/* Session list grouped by project */}
            <div class="flex-1 overflow-y-auto px-2 pb-2">
              {/* Free-standing sessions (no project) */}
              {sessions.filter(s => !s.project_id).map(s => renderSessionRow(s))}

              {/* Project groups — each project is a collapsible container with
                  its sessions nested inside. The header click toggles the
                  session list; the action buttons are separate so they do not
                  toggle on touch. */}
              {projects.map(p => {
                const isCollapsed = collapsedProjects.has(p.id)
                const projectSessions = sessions.filter(s => s.project_id === p.id)
                // The container does NOT clip. It must not: the "⋯" menu is
                // absolutely positioned and overflows the box, so any
                // `overflow-hidden` here cuts it off (measured: 21 of its 46 px
                // hidden, and worse on the last row). Clipping is not needed for
                // the collapse either — the session list is not rendered while
                // collapsed, and the list's own padding keeps the inner hover
                // backgrounds off the rounded corners. It was purely cosmetic and
                // it broke the menu.
                return (
                  <div key={p.id} class="mt-1.5 rounded-xl bg-white/[0.02] border border-white/[0.06]">
                    {/* Header: click toggles collapse. */}
                    <div
                      class="project-header group flex items-center gap-1.5 px-2.5 py-2 cursor-pointer select-none hover:bg-white/[0.04] transition-colors"
                      onClick={() => toggleProject(p.id)}
                      onTouchStart={(e) => startLongPress('project', p.id, p.title, e as unknown as Event)}
                      onTouchMove={cancelLongPress}
                      onTouchEnd={cancelLongPress}
                    >
                      {/* The arrow turns to follow the list: it points RIGHT
                          when the sessions are hidden and DOWN when they are
                          showing. One path that rotates, not two paths swapped,
                          because a swap cannot be animated. The rotation lives
                          in `.project-chevron` - this build disables Tailwind's
                          `transform` plugin, so a `rotate-90` utility would emit
                          a declaration the browser drops. */}
                      <svg
                        width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"
                        class={`project-chevron flex-none text-[#8a8a9a]${isCollapsed ? ' is-collapsed' : ''}`}
                      >
                        <polyline points="6 9 12 15 18 9" />
                      </svg>
                      {/* Folder icon */}
                      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="flex-none text-accent/60">
                        <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z" />
                      </svg>
                      {/* Title + facts, the same two-line shape as a session
                          row so the eye reads one pattern. */}
                      <div class="flex-1 min-w-0">
                        <div class="truncate font-semibold text-[13px] text-[#c8c8d2]">{p.title}</div>
                        {/* Facts line, labelled and coloured exactly like a
                            session's: `project` says this is the project's own
                            checkout rather than a session's, which is the
                            distinction the number is about. */}
                        <div class="flex items-center gap-x-1.5 gap-y-1 flex-wrap mt-1 text-[10px]">
                          {p.branch && (
                            <span
                              class="inline-flex items-center gap-1 flex-none px-1 py-px rounded bg-accent/10"
                              title={'Project checkout is on branch: ' + p.branch}
                            >
                              <span class="text-accent/50 uppercase tracking-wide text-[9px]">project</span>
                              <span class="font-mono text-accent truncate max-w-[7rem]">{shortBranch(p.branch)}</span>
                            </span>
                          )}
                          {!!p.worktrees && (
                            <span
                              class="inline-flex items-center gap-1 flex-none px-1 py-px rounded bg-[#a0a0f0]/10"
                              title={p.worktrees === 1 ? '1 session is working in its own worktree' : `${p.worktrees} sessions are working in their own worktrees`}
                            >
                              <span class="text-[#a0a0f0]/50 uppercase tracking-wide text-[9px]">worktrees</span>
                              <span class="tabular-nums text-[#a0a0f0]">{p.worktrees}</span>
                            </span>
                          )}
                          {!!p.changes && (
                            <span
                              class="inline-flex items-center gap-1 flex-none px-1 py-px rounded bg-[#f0a040]/10"
                              title={'The project checkout has ' + changesTitle(p.changes)}
                            >
                              <span class="text-[#f0a040]/50 uppercase tracking-wide text-[9px]">
                                {p.changes === 1 ? 'change' : 'changes'}
                              </span>
                              <span class="tabular-nums text-[#f0a040]">{p.changes}</span>
                            </span>
                          )}
                          {/* Session count: always present when there are
                              sessions, even collapsed, because it is what tells
                              the user what is inside without expanding. */}
                          {!!p.sessions && (
                            <span
                              class="inline-flex items-center gap-1 flex-none px-1 py-px rounded bg-white/5"
                              title={p.sessions === 1 ? 'This project has 1 session' : `This project has ${p.sessions} sessions`}
                            >
                              <span class="text-muted-foreground/60 uppercase tracking-wide text-[9px]">
                                {p.sessions === 1 ? 'session' : 'sessions'}
                              </span>
                              <span class="tabular-nums text-muted-foreground">{p.sessions}</span>
                            </span>
                          )}
                        </div>
                      </div>
                      {/* Action buttons: stopPropagation so they don't toggle. */}
                      <button
                        class="p-0.5 rounded hover:bg-white/10 opacity-0 group-hover:opacity-100 transition-opacity flex-none"
                        title="New session in project"
                        onClick={(e) => { e.stopPropagation(); createSession(p.id) }}
                      >
                        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                          <line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" />
                        </svg>
                      </button>
                      <div class={`row-actions relative flex-none${rowMenu?.id === p.id ? ' row-actions-open' : ''}`}>
                        <button
                          class="p-0.5 rounded hover:bg-white/10"
                          title="More actions"
                          onClick={(e) => {
                            e.stopPropagation()
                            if (rowMenu?.id === p.id) closeRowMenu()
                            else openRowMenu({ type: 'project', id: p.id, title: p.title })
                          }}
                        >
                          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                            <circle cx="12" cy="5" r="1" /><circle cx="12" cy="12" r="1" /><circle cx="12" cy="19" r="1" />
                          </svg>
                        </button>
                        {rowMenu?.id === p.id && (
                          <div
                            class={`row-menu absolute right-0 top-full mt-1 z-50 frosted rounded-xl border border-white/10 py-1 min-w-[140px]${rowMenuClosing ? ' row-menu-closing' : ''}`}
                            onClick={(e) => e.stopPropagation()}
                          >
                            <button
                              class="row-menu-item w-full flex items-center gap-2 px-3 py-2 text-sm text-danger hover:bg-danger/10"
                              onClick={(e) => { e.stopPropagation(); setConfirmDelete({ type: 'project', id: p.id, title: p.title }); closeRowMenu() }}
                            >
                              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                                <polyline points="3 6 5 6 21 6" /><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
                              </svg>
                              Delete
                            </button>
                          </div>
                        )}
                      </div>
                    </div>
                    {/* Session list: always rendered, so its height can
                        animate. Unmounting it would make the collapse snap,
                        which is exactly what was reported. The wrapper
                        collapses to `0fr` and `visibility` takes the rows out
                        of the tab order and the hit-testing once it has. */}
                    <div class={`project-sessions${isCollapsed ? ' is-collapsed' : ''}`}>
                      <div class="project-sessions-inner">
                        <div class="px-1.5 pb-1.5 space-y-0.5">
                          {projectSessions.map(s => renderSessionRow(s))}
                          {projectSessions.length === 0 && (
                            <div class="px-3 py-1.5 text-xs text-[#6a6a7a] italic">No sessions yet</div>
                          )}
                        </div>
                      </div>
                    </div>
                  </div>
                )
              })}
            </div>

            {/* Upgrade Motita button — shown only when a new version is available. */}
            {updateInfo?.update_available && (
              <div class="px-3 py-1 border-t border-white/5 flex-none">
                <button
                  class="w-full flex items-center gap-2 px-3 py-2.5 rounded-xl bg-accent/10 border border-accent/20 text-accent font-medium hover:bg-accent/20 active:scale-95 transition-all text-sm"
                  onClick={() => setShowUpgrade(true)}
                >
                  <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" /><polyline points="7 10 12 15 17 10" /><line x1="12" y1="15" x2="12" y2="3" />
                  </svg>
                  <span class="flex-1 text-left">Upgrade Motita</span>
                  <span class="text-[10px] font-mono opacity-70">{updateInfo.latest_version}</span>
                </button>
              </div>
            )}

            {/* Skill library and Scheduled tasks at the bottom */}
            <div class="px-3 py-2 border-t border-white/5 flex-none space-y-1">
              <button
                class="w-full flex items-center gap-2 px-3 py-2.5 rounded-xl hover:bg-white/5 transition-colors text-sm text-[#e8e8ea]"
                onClick={() => setShowSkillLibrary(true)}
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2z" /><path d="M22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z" />
                </svg>
                Skill library
              </button>
              <button
                class="w-full flex items-center gap-2 px-3 py-2.5 rounded-xl hover:bg-white/5 transition-colors text-sm text-[#e8e8ea]"
                onClick={() => { setShowScheduledTasks(true); void loadSchedules() }}
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <circle cx="12" cy="12" r="10" /><polyline points="12 6 12 12 16 14" />
                </svg>
                Scheduled tasks
              </button>
            </div>
          </aside>
        </>
      )}

      {/* Main column — header, conversation, composer. */}
      <div class="flex flex-col flex-1 min-w-0 h-[100dvh]">
        {/* Header — frosted glass over the background image.
            A phone is where this row runs out of room: title, status pill and
            provider/model pill all want width, and the two pills cannot shrink.
            overflow-hidden keeps a long model name from pushing the row wider
            than the viewport, the gaps tighten on the smallest screens, and the
            session title -- the only thing here with a real length -- gives up
            width first (min-w-0 on its group, truncate on the h1). */}
        <header class="frosted flex items-center gap-2 sm:gap-3 px-3 sm:px-5 py-2.5 sm:py-3 border-b border-white/5 flex-none overflow-hidden z-10">
          {!sidebarOpen && (
            <button
              class="p-1.5 rounded-lg hover:bg-white/5 transition-colors flex-none"
              onClick={() => setSidebarOpen(true)}
              aria-label="Open sidebar"
            >
              <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <line x1="3" y1="12" x2="21" y2="12" /><line x1="3" y1="6" x2="21" y2="6" /><line x1="3" y1="18" x2="21" y2="18" />
              </svg>
            </button>
          )}
          <div class="flex items-center gap-1.5 sm:gap-2 min-w-0 flex-1">
            <span class="text-accent text-base sm:text-lg flex-none">🐱</span>
            <h1 class="text-sm sm:text-base font-semibold tracking-wide truncate min-w-0">
              {sessions.find(s => s.id === sessionId)?.title || 'Motita'}
            </h1>
          </div>
          <span
            class={`flex-none whitespace-nowrap text-xs px-2 sm:px-2.5 py-1 rounded-full bg-black/20 border border-white/5 ${stateClass}`}
            aria-live="polite"
          >
            {stateText}
          </span>
          {config && (
            <button
              class="flex-none flex items-center gap-1 max-w-[38vw] max-[360px]:max-w-[30vw] sm:max-w-none text-xs px-2 sm:px-2.5 py-1 rounded-full bg-black/20 border border-white/5 text-[#9a9aaa] hover:bg-black/30 hover:border-accent/30 transition-colors cursor-pointer"
              title={`${config.provider} / ${config.model}`}
              onClick={openModelSwitcher}
            >
              <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent/60 flex-none">
                <rect x="2" y="3" width="20" height="14" rx="2" /><line x1="8" y1="21" x2="16" y2="21" /><line x1="12" y1="17" x2="12" y2="21" />
              </svg>
              <span class="truncate min-w-0">{config.provider} / {config.model}</span>
            </button>
          )}
        </header>

        {/* Conversation — the decorative gradient lives here only. */}
        <main
          ref={scrollRef}
          role="log"
          aria-live="polite"
          aria-label="conversation"
          class="chat-bg flex-1 overflow-y-auto px-3 py-4 sm:px-5 sm:py-5 flex flex-col gap-2.5 scroll-smooth"
        >
          {messages.map(m => (
            <div key={m.id} class={`msg ${m.role}${m.kind ? ' ' + m.kind : ''}`}>
              {m.role === 'agent' && !m.kind ? (
                <Markdown content={m.text} />
              ) : (
                <span>{m.text}</span>
              )}
            </div>
          ))}
          {activity && (
            <div class={`msg activity ${activityKind}`} style={{ whiteSpace: 'pre-wrap' }}>
              <span class={`spin-${activityKind || 'default'}`}></span>
              {activity}
            </div>
          )}
        </main>

        {/* Approval panel — solid opaque, above the gradient. */}
        {approval && (
          <div class="frosted flex-none px-4 sm:px-5 py-3.5 border-t-2 border-warning z-10">
            <h2 class="text-sm mb-2 text-warning">
              {approval.question ?? approval.reason ?? 'This needs your approval'}
            </h2>
            <pre class="whitespace-pre-wrap break-all max-h-[35vh] overflow-y-auto mb-2.5 p-3 bg-black/20 border border-white/5 rounded-lg font-mono text-[13px]">
              {approval.command ?? ''}
            </pre>
            <div class="flex gap-2">
              <button
                class="min-h-[44px] min-w-[44px] px-5 rounded-xl bg-accent text-white font-semibold active:scale-95 transition-transform"
                onClick={() => answerApproval(approval.id, true)}
              >
                Run it
              </button>
              <button
                class="min-h-[44px] min-w-[44px] px-5 rounded-xl border border-white/10 text-[#e8e8ea] active:scale-95 transition-transform"
                onClick={() => answerApproval(approval.id, false)}
              >
                No
              </button>
            </div>
          </div>
        )}

        {/* Composer — solid opaque background, no decorative gradient. */}
        <form
          class="frosted flex gap-2 px-3 py-2.5 sm:px-5 sm:py-3 border-t border-white/5 flex-none z-10 relative"
          onSubmit={handleSubmit}
        >
          {/* Slash command autocomplete popup — positioned above the textarea,
              inside the form so it tracks the input position. */}
          {slashPopup && (
            <div
              class="absolute bottom-full left-0 right-0 mb-1 rounded-xl border border-white/10 shadow-2xl overflow-hidden z-30 max-h-[160px] overflow-y-auto"
              style="background: rgba(10, 10, 16, 0.95); backdrop-filter: blur(12px); -webkit-backdrop-filter: blur(12px);"
            >
            {slashPopup.items.map((c, i) => {
              const groupColor: Record<string, string> = {
                mode: 'text-accent',
                action: 'text-[#f0a040]',
                session: 'text-[#40f0a0]',
                meta: 'text-[#a0a0f0]',
              }
              return (
                <div
                  key={c.name}
                  class={`flex items-center gap-2 px-3 py-2 cursor-pointer transition-colors ${i === slashPopup.index ? 'bg-white/10' : ''}`}
                  onClick={() => { if (!isCommandBlocked(c.name)) { setActiveTags(prev => [...prev, { name: c.name, group: c.group }]); setInput(''); setSlashPopup(null); document.getElementById('task')?.focus() } }}
                  onMouseEnter={() => setSlashPopup({ ...slashPopup, index: i })}
                >
                  <span class={`font-mono text-sm font-semibold flex-none w-20 ${groupColor[c.group] || 'text-[#e8e8ea]'}`}>{c.name}</span>
                  {c.aliases && c.aliases.length > 0 && <span class="text-xs text-[#6a6a7a] flex-none">{c.aliases.join(', ')}</span>}
                  <span class="flex-1 min-w-0 truncate text-xs text-[#9a9aaa]">{c.help}</span>
                </div>
              )
            })}
          </div>
        )}

          <label htmlFor="task" class="sr-only">Task</label>
          <div class="flex-1 flex flex-wrap items-center gap-1.5 min-h-[44px] max-h-[120px] p-2 rounded-2xl bg-black/30 border border-white/5 focus-within:border-accent backdrop-blur-sm overflow-y-auto">
            {activeTags.map((tag, ti) => (
              <span
                key={ti}
                class={`inline-flex items-center gap-1 px-2 py-0.5 rounded-md text-xs font-mono font-semibold flex-none ${
                  tag.group === 'mode' ? 'bg-accent/20 text-accent' :
                  tag.group === 'action' ? 'bg-[#f0a040]/20 text-[#f0a040]' :
                  tag.group === 'session' ? 'bg-[#40f0a0]/20 text-[#40f0a0]' :
                  'bg-[#a0a0f0]/20 text-[#a0a0f0]'
                }`}
              >
                {tag.name}
                <button
                  type="button"
                  class="ml-0.5 opacity-60 hover:opacity-100"
                  onClick={() => setActiveTags(prev => prev.filter((_, j) => j !== ti))}
                >
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                    <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                  </svg>
                </button>
              </span>
            ))}
            <textarea
              id="task"
              rows={1}
              value={input}
              onInput={(e) => {
              const val = (e.target as HTMLTextAreaElement).value
              setInput(val)
              // Slash command autocomplete: when the input starts with / and has
              // no space, filter commands. When it gets a space, check if the
              // first word is a valid command and convert it to a tag.
              if (val.startsWith('/') && !val.includes(' ')) {
                const query = val.toLowerCase()
                const matches = slashCommands.filter(c =>
                  !isCommandBlocked(c.name) &&
                  (c.name.toLowerCase().startsWith(query) ||
                  (c.aliases || []).some(a => a.toLowerCase().startsWith(query)))
                )
                setSlashPopup(matches.length > 0 ? { items: matches.slice(0, 8), index: 0 } : null)
              } else if (val.startsWith('/') && val.includes(' ')) {
                // Check if the first word is a valid command.
                const firstWord = val.split(' ')[0].toLowerCase()
                const match = slashCommands.find(c =>
                  c.name.toLowerCase() === firstWord ||
                  (c.aliases || []).some(a => a.toLowerCase() === firstWord)
                )
                if (match && !isCommandBlocked(match.name)) {
                  setActiveTags(prev => [...prev, { name: match.name, group: match.group }])
                  setInput(val.substring(firstWord.length + 1).replace(/^\s+/, ''))
                  setSlashPopup(null)
                  return
                }
                setSlashPopup(null)
              } else {
                setSlashPopup(null)
              }
            }}
            onKeyDown={handleKeydown}
            placeholder={activeTags.length > 0 ? 'argument…' : 'Ask for something…'}
            class="flex-1 min-h-[28px] max-h-[100px] px-1 py-1 bg-transparent border-0 outline-none ring-0 text-[#e8e8ea] resize-none focus:outline-none focus:ring-0 focus:border-0 font-sans text-[14px] leading-relaxed"
          />
          </div>
          <button
            type="submit"
            disabled={running}
            class="min-h-[44px] min-w-[44px] px-5 sm:px-6 rounded-2xl bg-accent text-white font-semibold disabled:opacity-40 active:scale-95 transition-transform flex items-center justify-center"
          >
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <line x1="22" y1="2" x2="11" y2="13" /><polygon points="22 2 15 22 11 13 2 9 22 2" />
            </svg>
          </button>
        </form>
      </div>

      {/* New project modal — title, description, folder or git URL. */}
      {showNewProject && (
        <div
          class="fixed inset-0 bg-black/60 backdrop-blur-sm z-50 flex items-center justify-center p-4"
          onClick={() => setShowNewProject(false)}
        >
          <div
            class="frosted rounded-2xl border border-white/10 w-full max-w-md p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div class="flex items-center gap-2 mb-4">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent">
                <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z" />
              </svg>
              <h2 class="text-base font-semibold">New project</h2>
              <button
                class="ml-auto p-1.5 rounded-lg hover:bg-white/5"
                onClick={() => setShowNewProject(false)}
                aria-label="Close"
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                </svg>
              </button>
            </div>

            <div class="space-y-4">
              <div>
                <label class="block text-sm text-[#9a9aaa] mb-1.5">Title <span class="text-danger">*</span></label>
                <input
                  class="w-full px-3 py-2.5 rounded-xl bg-black/30 border border-white/10 text-[#e8e8ea] focus:outline-none focus:border-accent"
                  value={newProjectTitle}
                  onInput={(e) => setNewProjectTitle((e.target as HTMLInputElement).value)}
                  placeholder="My project"
                  autoFocus
                />
              </div>
              <div>
                <label class="block text-sm text-[#9a9aaa] mb-1.5">Description (optional)</label>
                <input
                  class="w-full px-3 py-2.5 rounded-xl bg-black/30 border border-white/10 text-[#e8e8ea] focus:outline-none focus:border-accent"
                  value={newProjectDesc}
                  onInput={(e) => setNewProjectDesc((e.target as HTMLInputElement).value)}
                  placeholder="What this project is about"
                />
              </div>
              <div>
                <label class="block text-sm text-[#9a9aaa] mb-1.5">Folder name <span class="text-[#6a6a7a] text-xs">(auto from git URL if empty)</span></label>
                <input
                  class="w-full px-3 py-2.5 rounded-xl bg-black/30 border border-white/10 text-[#e8e8ea] focus:outline-none focus:border-accent font-mono text-sm"
                  value={newProjectDir}
                  onInput={(e) => setNewProjectDir((e.target as HTMLInputElement).value)}
                  placeholder="my-project"
                />
                <p class="text-xs text-[#6a6a7a] mt-1">A folder created under the workspace. Simple name, no paths.</p>
              </div>
              <div>
                <label class="block text-sm text-[#9a9aaa] mb-1.5">Git URL or SSH (optional — clones instead of creating a folder)</label>
                <input
                  class="w-full px-3 py-2.5 rounded-xl bg-black/30 border border-white/10 text-[#e8e8ea] focus:outline-none focus:border-accent font-mono text-sm"
                  value={newProjectGit}
                  onInput={(e) => setNewProjectGit((e.target as HTMLInputElement).value)}
                  placeholder="https://github.com/user/repo.git  or  git@github.com:user/repo.git"
                />
                <p class="text-xs text-[#6a6a7a] mt-1">HTTPS or SSH. When set, the repo is cloned into the folder name above.</p>
              </div>
            </div>

            {/* Clone status — shown while cloning. */}
            {creatingProject && (
              <div class="mt-4 flex items-center gap-2 text-sm text-[#9a9aaa]">
                <svg class="animate-spin flex-none text-accent" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                </svg>
                <span>Cloning repository…</span>
              </div>
            )}

            <div class="flex gap-2 mt-5">
              <button
                class="flex-1 min-h-[44px] px-5 rounded-xl bg-accent text-white font-semibold active:scale-95 transition-transform disabled:opacity-30 disabled:cursor-not-allowed disabled:saturate-0"
                onClick={() => createProject()}
                disabled={creatingProject || !newProjectTitle.trim() || (!newProjectDir.trim() && !newProjectGit.trim())}
              >
                {creatingProject && (
                  <svg class="animate-spin" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" style="display:inline-block;vertical-align:middle;margin-right:6px">
                    <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                  </svg>
                )}
                {creatingProject ? 'Cloning…' : 'Create'}
              </button>
              <button
                class="min-h-[44px] px-5 rounded-xl border border-white/10 text-[#e8e8ea] active:scale-95 transition-transform"
                onClick={() => setShowNewProject(false)}
              >
                Cancel
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Model Switcher modal — provider and model selection. */}
      {showModelSwitcher && (
        <div
          class="fixed inset-0 bg-black/60 backdrop-blur-sm z-50 flex items-center justify-center p-4"
          onClick={closeModelSwitcher}
        >
          <div
            class="frosted rounded-2xl border border-white/10 w-full max-w-md p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div class="flex items-center gap-2 mb-4">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent">
                <rect x="2" y="3" width="20" height="14" rx="2" /><line x1="8" y1="21" x2="16" y2="21" /><line x1="12" y1="17" x2="12" y2="21" />
              </svg>
              <h2 class="text-base font-semibold">Model</h2>
              <button
                class="ml-auto p-1.5 rounded-lg hover:bg-white/5"
                onClick={discardModelSwitcher}
                aria-label="Close"
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                </svg>
              </button>
            </div>

            <div class="space-y-4">
              <div>
                <label class="block text-sm text-[#9a9aaa] mb-1.5">Provider</label>
                <select
                  class="w-full px-3 py-2.5 rounded-xl bg-black/30 border border-white/10 text-[#e8e8ea] focus:outline-none focus:border-accent"
                  value={draftProvider}
                  onChange={(e) => {
                    const newProvider = (e.target as HTMLSelectElement).value
                    setDraftProvider(newProvider)
                    // The model is re-chosen rather than carried over: it belonged
                    // to the provider being left, and the new one may not serve it.
                    // Going back to the saved provider restores the saved model, so
                    // a provider round-trip leaves the draft exactly as it was.
                    setDraftModel(newProvider === (config?.provider || '') ? (config?.model || '') : firstModelOf(newProvider, providers))
                  }}
                >
                  {providers.map(p => (
                    <option key={p.id} value={p.id}>
                      {p.name}{p.is_current ? ' (active)' : ''}{p.key_present ? '' : ' — no key needed'}
                    </option>
                  ))}
                </select>
              </div>

              <div>
                <label class="block text-sm text-[#9a9aaa] mb-1.5">Model</label>
                {fetchingModels && draftProvider === (config?.provider || '') ? (
                  <div class="flex items-center gap-2 px-3 py-2.5 text-sm text-[#9a9aaa]">
                    <svg class="animate-spin text-accent" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                      <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                    </svg>
                    <span>Loading models...</span>
                  </div>
                ) : (
                  <select
                    class="w-full px-3 py-2.5 rounded-xl bg-black/30 border border-white/10 text-[#e8e8ea] focus:outline-none focus:border-accent font-mono text-sm"
                    value={draftModel}
                    onChange={(e) => setDraftModel((e.target as HTMLSelectElement).value)}
                  >
                    {modelOptions(draftProvider, draftModel, config?.provider || '', modelList, providers).map(m => (
                      <option key={m} value={m}>{m}</option>
                    ))}
                  </select>
                )}
              </div>

              {config && (
                <div class="text-xs text-[#6a6a7a] space-y-1 pt-2 border-t border-white/5">
                  <div>API key: {config.api_key_present ? '✓ set' : '✗ missing'}</div>
                  <div>Reasoning: {config.reasoning_enabled ? config.reasoning : 'off'}</div>
                </div>
              )}
            </div>

            <div class="flex gap-2 mt-5">
              <button
                class="flex-1 min-h-[44px] px-5 rounded-xl bg-accent text-white font-semibold active:scale-95 transition-transform disabled:opacity-30 disabled:cursor-not-allowed disabled:saturate-0"
                onClick={() => applyProviderModel()}
                disabled={savingConfig || !draftProvider || draftProvider === (config?.provider || '') && draftModel === (config?.model || '')}
              >
                {savingConfig && (
                  <svg class="animate-spin" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" style="display:inline-block;vertical-align:middle;margin-right:6px">
                    <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                  </svg>
                )}
                Done
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Confirm-delete modal — asks before deleting a session or project. */}
      {confirmDelete && (
        <div
          class="fixed inset-0 bg-black/60 backdrop-blur-sm z-50 flex items-center justify-center p-4"
          onClick={() => setConfirmDelete(null)}
        >
          <div
            class="frosted rounded-2xl border border-white/10 w-full max-w-sm p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div class="flex items-center gap-2 mb-3">
              <div class="w-10 h-10 rounded-full bg-danger/15 flex items-center justify-center flex-none">
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-danger">
                  <polyline points="3 6 5 6 21 6" /><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
                </svg>
              </div>
              <div class="flex-1 min-w-0">
                <h2 class="text-base font-semibold">Delete {confirmDelete.type}</h2>
                <p class="text-sm text-[#9a9aaa] truncate">{confirmDelete.title}</p>
              </div>
            </div>
            <p class="text-sm text-[#9a9aaa] mb-5">
              {confirmDelete.type === 'session'
                ? confirmDelete.id === 'default'
                  ? 'This is the default session. Deleting it will clear its history and reset its title, but the session itself will remain.'
                  : 'This conversation will be permanently deleted. This cannot be undone.'
                : 'This project and all its sessions will be permanently deleted. This cannot be undone.'}
            </p>
            <div class="flex gap-2">
              <button
                class="flex-1 min-h-[44px] px-5 rounded-xl bg-danger text-white font-semibold active:scale-95 transition-transform"
                onClick={async () => {
                  if (confirmDelete.type === 'session') {
                    await deleteSession(confirmDelete.id)
                  } else {
                    await deleteProject(confirmDelete.id)
                  }
                  setConfirmDelete(null)
                }}
              >
                Delete
              </button>
              <button
                class="min-h-[44px] px-5 rounded-xl border border-white/10 text-[#e8e8ea] active:scale-95 transition-transform"
                onClick={() => setConfirmDelete(null)}
              >
                Cancel
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Long-press context menu — appears on mobile when long-pressing a session or project. */}
      {contextMenu && (
        <div
          class="fixed inset-0 z-50"
          onClick={() => setContextMenu(null)}
        >
          <div
            class="frosted rounded-xl border border-white/10 shadow-2xl py-1 min-w-[160px]"
            style={{
              position: 'fixed',
              left: `${Math.min(contextMenu.x, window.innerWidth - 180)}px`,
              top: `${Math.min(contextMenu.y, window.innerHeight - 120)}px`,
            }}
            onClick={(e) => e.stopPropagation()}
          >
            <button
              class="w-full flex items-center gap-2 px-4 py-2.5 text-sm text-[#e8e8ea] hover:bg-white/5 transition-colors"
              onClick={() => {
                if (contextMenu.type === 'session') {
                  setRenamingId(contextMenu.id)
                  setRenameValue(contextMenu.title)
                }
                setContextMenu(null)
              }}
            >
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" /><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
              </svg>
              Rename
            </button>
            <button
              class="w-full flex items-center gap-2 px-4 py-2.5 text-sm text-danger hover:bg-danger/10 transition-colors"
              onClick={() => {
                setConfirmDelete({ type: contextMenu.type, id: contextMenu.id, title: contextMenu.title })
                setContextMenu(null)
              }}
            >
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <polyline points="3 6 5 6 21 6" /><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
              </svg>
              Delete
            </button>
          </div>
        </div>
      )}

      {/* Toast — auto-dismissing notification at the bottom of the screen.
          Centred with flex on a full-width wrapper rather than with
          translate-x-1/2: the slideUp keyframe animates `transform`, and a
          transform on the card would OVERRIDE the translate that centres it,
          which pushed the card off the right edge of a narrow window.
          `detail` is the second line: a merge names the commit it produced, and
          a failure says what did NOT change, which is the part a user actually
          needs. When the toast mentions a new version, clicking it opens the
          upgrade modal. */}
      {toast && (
        <div class="fixed bottom-6 inset-x-0 z-[60] flex justify-center px-4 pointer-events-none">
          <div
            class="frosted rounded-xl border px-4 py-3 flex items-start gap-3 max-w-md w-full pointer-events-auto"
            style={`animation: slideUp 0.3s ease-out; box-shadow: 0 20px 45px -10px rgba(0,0,0,0.75); border-color: ${toast.type === 'error' ? 'rgba(239,68,68,0.35)' : 'rgba(76,194,255,0.35)'}`}
          >
          {toast.type === 'error' ? (
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-danger flex-none mt-0.5">
              <circle cx="12" cy="12" r="10" /><line x1="12" y1="8" x2="12" y2="12" /><line x1="12" y1="16" x2="12.01" y2="16" />
            </svg>
          ) : updateInfo?.update_available && toast.message.includes('New version') ? (
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent flex-none mt-0.5 animate-pulse">
              <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" /><polyline points="7 10 12 15 17 10" /><line x1="12" y1="15" x2="12" y2="3" />
            </svg>
          ) : (
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" class="text-accent flex-none mt-0.5">
              <polyline points="20 6 9 17 4 12" />
            </svg>
          )}
          <div class="flex-1 min-w-0">
            <p class={`text-sm font-semibold ${toast.type === 'error' ? 'text-danger' : 'text-[#e8e8ea]'}`}>
              {toast.message}
            </p>
            {toast.detail && (
              <p class="text-xs text-[#9a9aaa] mt-1 leading-relaxed break-words">
                {toast.detail}
              </p>
            )}
            {toast.technical && (
              <div class="mt-2">
                <button
                  class="text-[11px] text-[#6a6a7a] hover:text-[#9a9aaa] underline decoration-dotted"
                  onClick={(e) => { e.stopPropagation(); setToastDetailsOpen(v => !v) }}
                >
                  {toastDetailsOpen ? 'Hide details' : 'Details'}
                </button>
                {toastDetailsOpen && (
                  <pre class="mt-1.5 text-[11px] text-[#8a8a9a] font-mono bg-black/30 rounded-lg p-2 overflow-x-auto whitespace-pre-wrap break-words max-h-32 overflow-y-auto">
                    {toast.technical}
                  </pre>
                )}
              </div>
            )}
            {updateInfo?.update_available && toast.message.includes('New version') && (
              <button
                class="mt-2 text-xs font-semibold text-accent hover:underline"
                onClick={(e) => { e.stopPropagation(); setShowUpgrade(true) }}
              >
                Upgrade now
              </button>
            )}
          </div>
          <button
            class="flex-none p-1 rounded-lg hover:bg-white/10 text-[#6a6a7a]"
            onClick={(e) => {
              e.stopPropagation()
              // A version toast is remembered as dismissed, so it does not
              // come back on the next poll; any other toast just goes away.
              if (updateInfo?.update_available && toast.message.includes('New version')) {
                dismissUpgradeToast()
              } else {
                setToast(null)
              }
            }}
            aria-label="Dismiss"
          >
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
            </svg>
          </button>
          </div>
        </div>
      )}

      {/* Upgrade modal — shows version info, a progress bar, and a confirm
          button. When the upgrade is running, the progress bar fills and the
          modal becomes non-dismissable. */}
      {showUpgrade && (
        <div
          class="fixed inset-0 bg-black/60 backdrop-blur-sm z-50 flex items-center justify-center p-4"
          onClick={() => { if (!upgradeBusy) setShowUpgrade(false) }}
        >
          <div
            class="frosted rounded-2xl border border-white/10 w-full max-w-md p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div class="flex items-center gap-2 mb-4">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent">
                <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" /><polyline points="7 10 12 15 17 10" /><line x1="12" y1="15" x2="12" y2="3" />
              </svg>
              <h2 class="text-base font-semibold">Upgrade Motita</h2>
              <button
                class="ml-auto p-1.5 rounded-lg hover:bg-white/5"
                onClick={() => { if (!upgradeBusy) setShowUpgrade(false) }}
                disabled={upgradeBusy}
                aria-label="Close"
                style={upgradeBusy ? 'opacity:0.3;cursor:not-allowed' : ''}
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                </svg>
              </button>
            </div>

            {!upgradeBusy && !upgradeProgress && (
              <>
                {/* Pre-upgrade confirmation view */}
                <div class="space-y-3">
                  <div class="flex items-center justify-between p-3 rounded-xl bg-black/20 border border-white/5">
                    <div>
                      <div class="text-xs text-[#6a6a7a] mb-0.5">Current version</div>
                      <div class="font-mono text-sm text-[#e8e8ea]">{updateInfo?.current_version || 'unknown'}</div>
                    </div>
                    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-[#6a6a7a]">
                      <line x1="5" y1="12" x2="19" y2="12" /><polyline points="12 5 19 12 12 19" />
                    </svg>
                    <div class="text-right">
                      <div class="text-xs text-[#6a6a7a] mb-0.5">Latest version</div>
                      <div class="font-mono text-sm text-accent">{updateInfo?.latest_version || 'unknown'}</div>
                    </div>
                  </div>
                  {updateInfo?.release_name && (
                    <div class="text-sm text-[#9a9aaa]">{updateInfo.release_name}</div>
                  )}
                  <p class="text-sm text-[#9a9aaa] leading-relaxed">
                    This will download the new binary from GitHub, verify its checksum, replace the
                    current executable, and <strong class="text-[#e8e8ea]">restart the gateway</strong>.
                    Any running tasks will be interrupted.
                  </p>
                  {updateInfo?.release_url && (
                    <a
                      href={updateInfo.release_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      class="text-xs text-accent hover:underline"
                    >
                      View release notes ↗
                    </a>
                  )}
                </div>
                <div class="flex gap-2 mt-5">
                  <button
                    class="flex-1 min-h-[44px] px-5 rounded-xl bg-accent text-white font-semibold active:scale-95 transition-transform flex items-center justify-center gap-2"
                    onClick={runUpgrade}
                  >
                    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                      <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" /><polyline points="7 10 12 15 17 10" /><line x1="12" y1="15" x2="12" y2="3" />
                    </svg>
                    Download &amp; Install
                  </button>
                  <button
                    class="min-h-[44px] px-5 rounded-xl border border-white/10 text-[#e8e8ea] active:scale-95 transition-transform"
                    onClick={() => setShowUpgrade(false)}
                  >
                    Cancel
                  </button>
                </div>
              </>
            )}

            {upgradeProgress && (
              <>
                {/* Upgrade in progress / done / error view */}
                <div class="space-y-4">
                  {/* Progress bar */}
                  <div class="relative">
                    <div class="h-3 rounded-full bg-black/30 border border-white/5 overflow-hidden">
                      <div
                        class="h-full rounded-full transition-all duration-300 ease-out"
                        style={{
                          width: `${upgradeProgress.percent || 0}%`,
                          background: upgradeProgress.stage === 'error'
                            ? 'linear-gradient(90deg, #ff6b6b, #ff8a8a)'
                            : upgradeProgress.stage === 'done'
                            ? 'linear-gradient(90deg, #4cc2ff, #6dd5ff)'
                            : 'linear-gradient(90deg, #4cc2ff, #6dd5ff)',
                          boxShadow: upgradeProgress.stage === 'downloading' || upgradeProgress.stage === 'installing'
                            ? '0 0 12px rgba(76,194,255,0.4)' : 'none',
                        }}
                      />
                    </div>
                    {/* Percentage label */}
                    <div class="flex justify-between mt-1.5">
                      <span class="text-xs text-[#9a9aaa]">
                        {upgradeProgress.stage === 'restarting'
                          ? 'Restarting…'
                          : upgradeProgress.stage === 'done'
                          ? 'Complete'
                          : upgradeProgress.stage === 'error'
                          ? 'Failed'
                          : `${upgradeProgress.percent || 0}%`}
                      </span>
                      <span class="text-xs font-mono text-[#6a6a7a]">
                        {upgradeProgress.version && `→ ${upgradeProgress.version}`}
                      </span>
                    </div>
                  </div>

                  {/* Stage indicator */}
                  <div class="flex items-center gap-2 text-sm">
                    {upgradeProgress.stage !== 'error' && upgradeProgress.stage !== 'done' && (
                      <svg class="animate-spin flex-none text-accent" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                        <path d="M21 12a9 9 0 1 1-6.219-8.56" />
                      </svg>
                    )}
                    {upgradeProgress.stage === 'done' && (
                      <svg class="text-accent flex-none" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
                        <polyline points="20 6 9 17 4 12" />
                      </svg>
                    )}
                    {upgradeProgress.stage === 'error' && (
                      <svg class="text-danger flex-none" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                        <circle cx="12" cy="12" r="10" /><line x1="12" y1="8" x2="12" y2="12" /><line x1="12" y1="16" x2="12.01" y2="16" />
                      </svg>
                    )}
                    <span class={upgradeProgress.stage === 'error' ? 'text-danger' : 'text-[#e8e8ea]'}>
                      {upgradeProgress.message}
                    </span>
                  </div>

                  {/* Error details — the same two-layer shape as the toast: a
                      plain-language explanation the user can act on, with the
                      raw text folded away behind "Details" for a bug report.
                      A bare exception string is not an explanation. */}
                  {upgradeError && (
                    <div class="p-3 rounded-xl bg-danger/10 border border-danger/20">
                      <div class="flex items-start gap-2">
                        <svg class="text-danger flex-none mt-0.5" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                          <circle cx="12" cy="12" r="10" /><line x1="12" y1="8" x2="12" y2="12" /><line x1="12" y1="16" x2="12.01" y2="16" />
                        </svg>
                        <div class="flex-1 min-w-0">
                          <p class="text-sm font-medium text-danger">The upgrade did not finish</p>
                          <p class="text-xs text-[#9a9aaa] mt-1 leading-relaxed">
                            The gateway is still running the version it had before. Nothing was replaced, so you can keep using it and try again.
                          </p>
                          <button
                            class="text-xs text-[#6a6a7a] hover:text-[#9a9aaa] mt-1.5 underline underline-offset-2"
                            onClick={() => setShowUpgradeError(v => !v)}
                          >
                            {showUpgradeError ? 'Hide details' : 'Details'}
                          </button>
                          {showUpgradeError && (
                            <pre class="text-xs text-danger whitespace-pre-wrap font-mono mt-2 p-2 rounded-lg bg-black/40">{upgradeError}</pre>
                          )}
                        </div>
                      </div>
                    </div>
                  )}

                  {/* Restarting note */}
                  {upgradeProgress.stage === 'restarting' && (
                    <p class="text-xs text-[#6a6a7a] leading-relaxed">
                      The gateway is restarting. This page will reload automatically once it is back.
                    </p>
                  )}
                  {upgradeProgress.stage === 'done' && (
                    <p class="text-xs text-[#6a6a7a] leading-relaxed">
                      The upgrade is complete and the gateway is restarting. This page will reload
                      automatically in a moment.
                    </p>
                  )}
                </div>

                {/* Close button — only available when not busy */}
                {!upgradeBusy && upgradeProgress.stage !== 'restarting' && (
                  <div class="flex gap-2 mt-5">
                    <button
                      class="flex-1 min-h-[44px] px-5 rounded-xl border border-white/10 text-[#e8e8ea] active:scale-95 transition-transform"
                      onClick={() => {
                        setShowUpgrade(false)
                        setUpgradeProgress(null)
                        setUpgradeError('')
                        setShowUpgradeError(false)
                      }}
                    >
                      {upgradeProgress.stage === 'error' ? 'Close' : 'Done'}
                    </button>
                  </div>
                )}
              </>
            )}
          </div>
        </div>
      )}

      {showSkillLibrary && (
        <div
          class="fixed inset-0 bg-black/60 backdrop-blur-sm z-50 flex items-center justify-center p-4"
          onClick={() => setShowSkillLibrary(false)}
        >
          <div
            class="frosted rounded-2xl border border-white/10 w-full max-w-md p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div class="flex items-center gap-2 mb-4">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent">
                <path d="M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2z" /><path d="M22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z" />
              </svg>
              <h2 class="text-base font-semibold">Skill library</h2>
              <button
                class="ml-auto p-1.5 rounded-lg hover:bg-white/5"
                onClick={() => setShowSkillLibrary(false)}
                aria-label="Close"
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                </svg>
              </button>
            </div>
            <p class="text-sm text-[#9a9aaa]">The skill library browser is not yet available.</p>
            <div class="flex gap-2 mt-5">
              <button
                class="flex-1 min-h-[44px] px-5 rounded-xl border border-white/10 text-[#e8e8ea] active:scale-95 transition-transform"
                onClick={() => setShowSkillLibrary(false)}
              >
                Close
              </button>
            </div>
          </div>
        </div>
      )}

      {showScheduledTasks && (
        <div
          class="fixed inset-0 bg-black/60 backdrop-blur-sm z-50 flex items-center justify-center p-4"
          onClick={() => setShowScheduledTasks(false)}
        >
          <div
            class="tasks-panel frosted rounded-2xl border border-white/10 w-full max-w-md p-5 shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div class="tasks-head flex items-center gap-2 mb-4">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-accent">
                <circle cx="12" cy="12" r="10" /><polyline points="12 6 12 12 16 14" />
              </svg>
              <h2 class="text-base font-semibold">Scheduled tasks</h2>
              <button
                class="ml-auto p-1.5 rounded-lg hover:bg-white/5"
                onClick={() => setShowScheduledTasks(false)}
                aria-label="Close"
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                  <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
                </svg>
              </button>
            </div>

            {/* On a phone this is one column, list first and the create form below it -
                the order that fits a narrow screen. On a screen with room the form
                moves to the LEFT of the list (see .tasks-body in index.css), which is
                the order a person works in: write the task, then watch it land in the
                list beside it. The DOM keeps the phone's order and the two columns are
                PLACED by the stylesheet, so neither layout duplicates the markup. */}
            <div class="tasks-body">
              {/* The list is what makes the feature usable: a task whose next run and
                  last outcome a user cannot see is a task they cannot trust, so both
                  are on the row and neither is behind a click. */}
              <div class="tasks-list-column">
                <p class="tasks-heading">Scheduled ({scheduledTasks.length})</p>
                <div class="tasks-list space-y-2 max-h-[45vh] overflow-y-auto">
                  {scheduledTasks.length === 0 && (
                    <p class="text-sm text-[#9a9aaa]">
                      No scheduled tasks yet. One created here fires into the conversation you are in.
                    </p>
                  )}
                  {scheduledTasks.map((t) => {
                    const cd = taskCountdown(t, nowMs, clockSkew)
                    return (
                      <div key={t.id} class="rounded-xl border border-white/10 p-3">
                        <div class="flex items-center gap-2">
                          <span class="flex-1 text-sm text-[#e8e8ea] truncate">{t.title}</span>
                          {/* The cadence stays visible, but as a TAG: what a person
                              wants off a row is when it fires NEXT, and `every` is
                              the setting they typed when they created it. */}
                          <span class="task-tag">every {durationOf(t.every).short}</span>
                        </div>
                        {/* The countdown is the row's headline figure: the tag says what
                            the task is SET to, this says how long is left. A paused task
                            shows none - its next run is a projection, and a ticker over a
                            task that will not fire would be a lie with a pulse. */}
                        {cd && (
                          <p class="text-xs text-[#9a9aaa] mt-1.5">
                            {cd.overdue ? 'due now' : 'fires in '}
                            {!cd.overdue && <span class="task-countdown">{cd.text}</span>}
                          </p>
                        )}
                        {!t.enabled && (
                          <p class="text-xs text-[#9a9aaa] mt-1.5">
                            Paused — last {new Date(t.last_run || t.created).toLocaleString()}
                          </p>
                        )}
                        {t.last_outcome && (
                          <p class="text-xs text-[#6a6a7a] mt-1 break-words">{t.last_outcome}</p>
                        )}
                        <div class="flex gap-2 mt-2">
                          <button
                            class="min-h-[44px] px-3 rounded-lg border border-white/10 text-xs text-[#e8e8ea] active:scale-95 transition-transform"
                            onClick={() => void runScheduleNow(t)}
                          >
                            Run now
                          </button>
                          <button
                            class="min-h-[44px] px-3 rounded-lg border border-white/10 text-xs text-[#e8e8ea] active:scale-95 transition-transform"
                            onClick={() => void toggleSchedule(t)}
                          >
                            {t.enabled ? 'Pause' : 'Resume'}
                          </button>
                          <button
                            class="min-h-[44px] px-3 rounded-lg border border-danger/30 text-xs text-danger active:scale-95 transition-transform ml-auto"
                            onClick={() => void deleteSchedule(t)}
                          >
                            Delete
                          </button>
                        </div>
                      </div>
                    )
                  })}
                </div>
              </div>

              {/* The create form. Cadence is free text because the gateway validates it
                  and says what is wrong: a fixed dropdown would be a second copy of the
                  rule. */}
              <div class="tasks-form pt-4 border-t border-white/10 space-y-2">
                <p class="tasks-heading">New task</p>
                <input
                  class="w-full px-3 py-2 rounded-lg bg-black/30 border border-white/10 text-sm text-[#e8e8ea]"
                  placeholder="Title"
                  value={newTaskTitle}
                  onInput={(e) => setNewTaskTitle((e.target as HTMLInputElement).value)}
                />
                <textarea
                  class="w-full px-3 py-2 rounded-lg bg-black/30 border border-white/10 text-sm text-[#e8e8ea] resize-none"
                  rows={3}
                  placeholder="What should it do?"
                  value={newTaskText}
                  onInput={(e) => setNewTaskText((e.target as HTMLTextAreaElement).value)}
                />
                <div class="flex gap-2">
                  <input
                    class="flex-1 px-3 py-2 rounded-lg bg-black/30 border border-white/10 text-sm text-[#e8e8ea] font-mono"
                    placeholder="24h"
                    value={newTaskEvery}
                    onInput={(e) => setNewTaskEvery((e.target as HTMLInputElement).value)}
                  />
                  {/* min-w-0: a flex item's automatic minimum size is its longest word, and a
                      <select> is a replaced element whose text and arrow give it a real
                      min-content width - wide enough ("Plan (read-only)") that the row was 3px
                      past the form and grew a horizontal scrollbar over the whole left column.
                      It keeps its intrinsic width while the column is wide enough and gives way
                      before pushing anything out of the card. */}
                  <select
                    class="min-w-0 px-3 py-2 rounded-lg bg-black/30 border border-white/10 text-sm text-[#e8e8ea]"
                    value={newTaskKind}
                    onChange={(e) => setNewTaskKind((e.target as HTMLSelectElement).value as 'task' | 'plan')}
                  >
                    <option value="task">Task</option>
                    <option value="plan">Plan (read-only)</option>
                  </select>
                </div>
                <button
                  class="w-full min-h-[44px] px-5 rounded-xl bg-accent/20 border border-accent/30 text-[#e8e8ea] active:scale-95 transition-transform disabled:opacity-50"
                  disabled={scheduledBusy || !newTaskTitle.trim() || !newTaskText.trim()}
                  onClick={() => void createSchedule()}
                >
                  {scheduledBusy ? 'Creating…' : 'Create'}
                </button>
              </div>
            </div>

            <div class="tasks-foot flex gap-2 mt-5">
              <button
                class="flex-1 min-h-[44px] px-5 rounded-xl border border-white/10 text-[#e8e8ea] active:scale-95 transition-transform"
                onClick={() => setShowScheduledTasks(false)}
              >
                Close
              </button>
            </div>
          </div>
        </div>
      )}

    </div>
  )
}