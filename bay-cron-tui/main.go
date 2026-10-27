// bay-cron-tui: a terminal dashboard for the bay-cron container.
//
// Shows the scheduled jobs (parsed from bay-cron's own log output), the
// container's state, and a live tail of its logs, and runs start / stop /
// pull / build / pull & build against the bay-cron project directory.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // embed zone data so CRON_TZ / America/New_York always resolve

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/robfig/cron/v3"
)

// ───────────────────────────── config & actions ─────────────────────────────

type config struct {
	dir       string
	container string
	lines     int
}

type action struct {
	key   string
	label string
	steps []string
}

const compose = "docker compose --ansi never --progress plain"

var actions = []action{
	{"s", "start", []string{compose + " up -d"}},
	{"x", "stop", []string{compose + " stop"}},
	{"p", "pull", []string{"git pull --ff-only"}},
	{"b", "build", []string{compose + " up -d --build"}},
	{"u", "pull & build", []string{"git pull --ff-only", compose + " up -d --build"}},
}

// ───────────────────────────────── styles ──────────────────────────────────

var (
	accent = lipgloss.Color("#E5484D")
	dim    = lipgloss.Color("244")
	green  = lipgloss.Color("42")
	yellow = lipgloss.Color("214")
	red    = lipgloss.Color("196")

	bannerStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(dim)
	headStyle   = lipgloss.NewStyle().Foreground(dim).Bold(true)
	boldStyle   = lipgloss.NewStyle().Bold(true)
	keyStyle    = lipgloss.NewStyle().Foreground(accent).Bold(true)
	okStyle     = lipgloss.NewStyle().Foreground(green)
	warnStyle   = lipgloss.NewStyle().Foreground(yellow)
	errStyle    = lipgloss.NewStyle().Foreground(red)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(dim).Padding(0, 1)
)

// ANSI Shadow glyphs, 6 rows each, every row of a glyph the same width.
var glyphs = map[rune][6]string{
	'B': {"██████╗ ", "██╔══██╗", "██████╔╝", "██╔══██╗", "██████╔╝", "╚═════╝ "},
	'A': {" █████╗ ", "██╔══██╗", "███████║", "██╔══██║", "██║  ██║", "╚═╝  ╚═╝"},
	'Y': {"██╗   ██╗", "╚██╗ ██╔╝", " ╚████╔╝ ", "  ╚██╔╝  ", "   ██║   ", "   ╚═╝   "},
	'-': {"      ", "      ", "█████╗", "╚════╝", "      ", "      "},
	'C': {" ██████╗", "██╔════╝", "██║     ", "██║     ", "╚██████╗", " ╚═════╝"},
	'R': {"██████╗ ", "██╔══██╗", "██████╔╝", "██╔══██╗", "██║  ██║", "╚═╝  ╚═╝"},
	'O': {" ██████╗ ", "██╔═══██╗", "██║   ██║", "██║   ██║", "╚██████╔╝", " ╚═════╝ "},
	'N': {"███╗   ██╗", "████╗  ██║", "██╔██╗ ██║", "██║╚██╗██║", "██║ ╚████║", "╚═╝  ╚═══╝"},
}

const bannerText = "BAY-CRON"
const bannerMinWidth = 70

func bigText(s string) []string {
	rows := make([]string, 6)
	for _, r := range s {
		g, ok := glyphs[r]
		if !ok {
			continue
		}
		for i := range rows {
			rows[i] += g[i]
		}
	}
	return rows
}

// ─────────────────────────────── log parsing ───────────────────────────────

var (
	ansiRe  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)
	tsRe    = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2}T[^\]]+)\]\s*(.*)$`)
	schedRe = regexp.MustCompile(`^scheduled\s+(\S+)\s+\(([^)]*)\)(?:\s*(?:→|->)\s*([^,\s]+))?`)
	failRe  = regexp.MustCompile(`(?i)\b(fail(ed|ure|s)?|error|timed?[ -]?out|abort(ed)?|refused|unreachable)\b|\b[45]\d\d\b`)
)

func clean(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\t", "    ")
}

type job struct {
	Name     string
	Expr     string
	TZ       string
	Endpoint string
	LastRun  time.Time
	LastLine string
	Failed   bool
	sched    cron.Schedule
	loc      *time.Location
}

// "0 9 * * * America/New_York" -> ("0 9 * * *", "America/New_York")
func splitSchedule(s string) (expr, tz string) {
	f := strings.Fields(s)
	if len(f) > 5 {
		last := f[len(f)-1]
		if strings.Contains(last, "/") || last == "UTC" {
			return strings.Join(f[:len(f)-1], " "), last
		}
	}
	return strings.Join(f, " "), ""
}

func isWordByte(c byte) bool {
	return c == '-' || c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// mentions reports whether token appears in body as a whole word.
func mentions(body, token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(body); {
		k := strings.Index(body[i:], token)
		if k < 0 {
			return false
		}
		k += i
		end := k + len(token)
		if (k == 0 || !isWordByte(body[k-1])) && (end == len(body) || !isWordByte(body[end])) {
			return true
		}
		i = k + 1
	}
	return false
}

// logStore holds the container's log lines plus the jobs derived from them.
type logStore struct {
	mu      sync.Mutex
	lines   []string
	max     int
	jobs    map[string]*job
	order   []string
	version int
}

func newLogStore(max int) *logStore {
	return &logStore{max: max, jobs: map[string]*job{}}
}

func (s *logStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = nil
	s.jobs = map[string]*job{}
	s.order = nil
	s.version++
}

func (s *logStore) ver() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *logStore) add(raw string) {
	line := clean(raw)
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lines = append(s.lines, line)
	if len(s.lines) > 2*s.max {
		s.lines = append([]string(nil), s.lines[len(s.lines)-s.max:]...)
	}
	s.version++

	m := tsRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		return
	}
	body := m[2]

	// "scheduled <name> (<cron> <tz>) → <endpoint>, next run ..."
	if sm := schedRe.FindStringSubmatch(body); sm != nil {
		j := s.jobs[sm[1]]
		if j == nil {
			j = &job{Name: sm[1]}
			s.jobs[sm[1]] = j
			s.order = append(s.order, sm[1])
		}
		j.Expr, j.TZ = splitSchedule(sm[2])
		j.Endpoint = sm[3]
		j.loc = time.Local
		spec := j.Expr
		if j.TZ != "" {
			if l, err := time.LoadLocation(j.TZ); err == nil {
				j.loc = l
			}
			spec = "CRON_TZ=" + j.TZ + " " + spec
		}
		j.sched, _ = cron.ParseStandard(spec)
		return
	}

	// Any other line naming a job (or its endpoint) counts as its latest run.
	for _, name := range s.order {
		j := s.jobs[name]
		if mentions(body, j.Name) || mentions(body, j.Endpoint) {
			j.LastRun = ts
			j.LastLine = body
			j.Failed = failRe.MatchString(body)
		}
	}
}

func (s *logStore) snapshot() ([]string, []job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := max(0, len(s.lines)-s.max)
	lines := append([]string(nil), s.lines[start:]...)
	jobs := make([]job, 0, len(s.order))
	for _, n := range s.order {
		jobs = append(jobs, *s.jobs[n])
	}
	return lines, jobs
}

// lineBuf is a plain capped buffer, used for action output.
type lineBuf struct {
	mu      sync.Mutex
	lines   []string
	max     int
	version int
}

func (b *lineBuf) add(l string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, clean(l))
	if len(b.lines) > 2*b.max {
		b.lines = append([]string(nil), b.lines[len(b.lines)-b.max:]...)
	}
	b.version++
}

func (b *lineBuf) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = nil
	b.version++
}

func (b *lineBuf) ver() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}

func (b *lineBuf) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := max(0, len(b.lines)-b.max)
	return append([]string(nil), b.lines[start:]...)
}

// ─────────────────────────────── processes ─────────────────────────────────

// runLines runs cmd, feeding every stdout/stderr line to onLine.
func runLines(cmd *exec.Cmd, onLine func(string), started func(*exec.Cmd)) error {
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	if started != nil {
		started(cmd)
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		done <- err
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		onLine(sc.Text())
	}
	_, _ = io.Copy(io.Discard, pr) // drain if the scanner bailed on a huge line
	return <-done
}

// follower keeps `docker logs` attached to the container, re-attaching when
// it is stopped, started, or recreated by a rebuild.
type follower struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

func (f *follower) kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
	}
}

func (f *follower) logs(container string, follow bool, s *logStore) {
	args := []string{"logs"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, container)
	cmd := exec.Command("docker", args...)
	err := runLines(cmd, s.add, func(c *exec.Cmd) {
		f.mu.Lock()
		f.cmd = c
		f.mu.Unlock()
	})
	f.mu.Lock()
	f.cmd = nil
	f.mu.Unlock()
	if err != nil && !follow {
		s.add("[tui] docker logs: " + err.Error())
	}
}

func (f *follower) loop(container string, s *logStore) {
	lastKey := ""
	for {
		out, err := exec.Command("docker", "inspect", "-f",
			"{{.State.Status}}|{{.State.StartedAt}}|{{.Id}}", container).Output()
		if err != nil {
			if lastKey != "missing" {
				s.reset()
				s.add("[tui] container " + container + " not found. Press s to start or b to build it.")
				lastKey = "missing"
			}
			time.Sleep(2 * time.Second)
			continue
		}
		key := strings.TrimSpace(string(out))
		if !strings.HasPrefix(key, "running|") {
			// Stopped: show its last logs once, then wait for a state change.
			if key != lastKey {
				s.reset()
				f.logs(container, false, s)
				lastKey = key
			}
			time.Sleep(2 * time.Second)
			continue
		}
		s.reset()
		f.logs(container, true, s) // blocks until the container stops
		lastKey = ""
		time.Sleep(time.Second)
	}
}

// ─────────────────────────────── tea messages ──────────────────────────────

type tickMsg time.Time
type gitMsg string

type statusMsg struct {
	State    string
	Started  time.Time
	Restarts int
}

type actionDoneMsg struct {
	label string
	err   error
	dur   time.Duration
}

func tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func fetchStatus(container string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("docker", "inspect", "-f",
			"{{.State.Status}}|{{.State.StartedAt}}|{{.RestartCount}}", container).Output()
		if err != nil {
			return statusMsg{State: "not created"}
		}
		parts := strings.Split(strings.TrimSpace(string(out)), "|")
		st := statusMsg{State: parts[0]}
		if len(parts) == 3 {
			st.Started, _ = time.Parse(time.RFC3339Nano, parts[1])
			st.Restarts, _ = strconv.Atoi(parts[2])
		}
		return st
	}
}

func fetchGit(dir string) tea.Cmd {
	return func() tea.Msg {
		commit, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%h %s").Output()
		if err != nil {
			return gitMsg("")
		}
		branch, _ := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		return gitMsg(strings.TrimSpace(string(branch)) + " @ " + strings.TrimSpace(string(commit)))
	}
}

func runAction(dir string, a action, buf *lineBuf) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		for _, step := range a.steps {
			buf.add("$ " + step)
			cmd := exec.Command("sh", "-c", step)
			cmd.Dir = dir
			// Never let git or ssh prompt; a prompt would hang behind the TUI.
			cmd.Env = append(os.Environ(),
				"GIT_TERMINAL_PROMPT=0",
				"GIT_SSH_COMMAND=ssh -o BatchMode=yes",
				"NO_COLOR=1",
			)
			if err := runLines(cmd, buf.add, nil); err != nil {
				buf.add("✗ " + err.Error())
				return actionDoneMsg{a.label, err, time.Since(start)}
			}
		}
		buf.add("✓ done")
		return actionDoneMsg{a.label, nil, time.Since(start)}
	}
}

// ────────────────────────────────── model ──────────────────────────────────

type viewMode int

const (
	viewLogs viewMode = iota
	viewAction
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type model struct {
	cfg    config
	logs   *logStore
	actBuf *lineBuf

	vp     viewport.Model
	vpInit bool
	width  int
	height int
	ready  bool
	follow bool
	mode   viewMode
	ticks  int

	logsVer  int
	actVer   int
	logLines []string
	actLines []string
	jobs     []job

	status statusMsg
	git    string

	busy        bool
	actionLabel string
	actionErr   error
	result      string
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(tick(), fetchStatus(m.cfg.container), fetchGit(m.cfg.dir))
}

func (m *model) bannerHeight() int {
	if m.width >= bannerMinWidth {
		return 6
	}
	return 1
}

func (m *model) layout() {
	if m.width == 0 {
		return
	}
	rows := max(1, len(m.jobs))
	// banner + status + jobs box (border 2 + header 1 + rows) + logs box chrome (border 2 + title 1) + help
	used := m.bannerHeight() + 1 + (rows + 3) + 3 + 1
	h := max(3, m.height-used)
	w := max(10, m.width-4)
	if !m.vpInit {
		m.vp = viewport.New(w, h)
		m.vpInit = true
	} else {
		m.vp.Width = w
		m.vp.Height = h
	}
}

func (m *model) refresh(force bool) {
	changed := force
	if v := m.logs.ver(); v != m.logsVer {
		m.logsVer = v
		lines, jobs := m.logs.snapshot()
		m.logLines = lines
		resize := len(jobs) != len(m.jobs)
		m.jobs = jobs
		if resize {
			m.layout()
		}
		if m.mode == viewLogs {
			changed = true
		}
	}
	if v := m.actBuf.ver(); v != m.actVer {
		m.actVer = v
		m.actLines = m.actBuf.snapshot()
		if m.mode == viewAction {
			changed = true
		}
	}
	if !changed || !m.ready {
		return
	}
	src := m.logLines
	if m.mode == viewAction {
		src = m.actLines
	}
	out := make([]string, len(src))
	for i, l := range src {
		out[i] = runewidth.Truncate(l, m.vp.Width, "…")
	}
	m.vp.SetContent(strings.Join(out, "\n"))
	if m.follow {
		m.vp.GotoBottom()
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.layout()
		m.refresh(true)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		m.ticks++
		m.refresh(false)
		cmds := []tea.Cmd{tick()}
		if m.ticks%10 == 0 {
			cmds = append(cmds, fetchStatus(m.cfg.container))
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.status = msg

	case gitMsg:
		m.git = string(msg)

	case actionDoneMsg:
		m.busy = false
		m.actionErr = msg.err
		if msg.err != nil {
			m.result = errStyle.Render("✗ " + msg.label + " failed")
		} else {
			m.result = okStyle.Render(fmt.Sprintf("✓ %s done in %s", msg.label, msg.dur.Round(time.Second)))
		}
		m.refresh(true)
		return m, tea.Batch(fetchStatus(m.cfg.container), fetchGit(m.cfg.dir))
	}
	return m, nil
}

func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		if m.mode == viewLogs {
			m.mode = viewAction
		} else {
			m.mode = viewLogs
		}
		m.follow = true
		m.refresh(true)
		return m, nil
	case "f", "G", "end":
		m.follow = true
		m.vp.GotoBottom()
		return m, nil
	case "g", "home":
		m.follow = false
		m.vp.GotoTop()
		return m, nil
	}

	for _, a := range actions {
		if k.String() != a.key {
			continue
		}
		if m.busy {
			return m, nil
		}
		m.busy = true
		m.actionLabel = a.label
		m.actionErr = nil
		m.result = ""
		m.mode = viewAction
		m.follow = true
		m.actBuf.reset()
		m.refresh(true)
		return m, runAction(m.cfg.dir, a, m.actBuf)
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(k)
	m.follow = m.vp.AtBottom()
	return m, cmd
}

// ────────────────────────────────── view ───────────────────────────────────

func pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return runewidth.FillRight(runewidth.Truncate(s, w-1, "…"), w)
}

func human(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func (m *model) renderBanner() string {
	if m.width < bannerMinWidth {
		return bannerStyle.Render(bannerText)
	}
	rows := bigText(bannerText)
	for i, r := range rows {
		rows[i] = bannerStyle.Render(r)
	}
	if m.width >= 96 {
		rows[5] += dimStyle.Render("   FinishLine cron runner")
	}
	return strings.Join(rows, "\n")
}

func (m *model) renderStatus() string {
	var dot string
	switch m.status.State {
	case "running":
		dot = okStyle.Render("●")
	case "restarting", "created", "paused":
		dot = warnStyle.Render("●")
	case "":
		dot = dimStyle.Render("●")
	default:
		dot = errStyle.Render("●")
	}
	state := m.status.State
	if state == "" {
		state = "checking…"
	}
	parts := []string{dot + " " + boldStyle.Render(state)}
	if m.status.State == "running" && !m.status.Started.IsZero() {
		parts = append(parts, dimStyle.Render("up "+human(time.Since(m.status.Started))))
	}
	if m.status.Restarts > 0 {
		parts = append(parts, warnStyle.Render(fmt.Sprintf("%d restarts", m.status.Restarts)))
	}
	if m.git != "" {
		parts = append(parts, dimStyle.Render("⎇ "+runewidth.Truncate(m.git, max(10, m.width-45), "…")))
	}
	return " " + strings.Join(parts, dimStyle.Render("  ·  "))
}

func scheduleLabel(j job) string {
	if j.TZ == "" {
		return j.Expr
	}
	return j.Expr + "  " + j.TZ
}

func (m *model) renderJobs() string {
	cw := m.width - 4
	nameW, schedW := len("NAME"), len("SCHEDULE")
	for _, j := range m.jobs {
		nameW = max(nameW, runewidth.StringWidth(j.Name))
		schedW = max(schedW, runewidth.StringWidth(scheduleLabel(j)))
	}
	nameW += 2
	schedW += 2
	timeW := 28
	if cw < nameW+schedW+2*timeW {
		timeW = max(12, (cw-nameW-schedW)/2)
	}
	outW := cw - nameW - schedW - 2*timeW
	showOut := outW > 8

	header := pad("NAME", nameW) + pad("SCHEDULE", schedW) + pad("LAST RUN", timeW) + pad("NEXT RUN", timeW)
	if showOut {
		header += pad("LAST OUTPUT", outW)
	}
	lines := []string{headStyle.Render(header)}

	if len(m.jobs) == 0 {
		lines = append(lines, dimStyle.Render(pad("No jobs yet. Waiting for bay-cron's startup \"scheduled\" lines.", cw)))
	}

	now := time.Now()
	for _, j := range m.jobs {
		loc := j.loc
		if loc == nil {
			loc = time.Local
		}

		last := dimStyle.Render(pad("never (since start)", timeW))
		if !j.LastRun.IsZero() {
			txt := pad(j.LastRun.In(loc).Format("Mon 15:04:05")+" ("+human(now.Sub(j.LastRun))+" ago)", timeW)
			if j.Failed {
				last = errStyle.Render(txt)
			} else {
				last = okStyle.Render(txt)
			}
		}

		next := dimStyle.Render(pad("—", timeW))
		if j.sched != nil {
			n := j.sched.Next(now)
			next = pad(n.In(loc).Format("Mon 15:04 MST")+" (in "+human(n.Sub(now))+")", timeW)
		}

		row := boldStyle.Render(pad(j.Name, nameW)) + pad(scheduleLabel(j), schedW) + last + next
		if showOut {
			row += dimStyle.Render(pad(j.LastLine, outW))
		}
		lines = append(lines, row)
	}
	return boxStyle.Width(m.width - 2).Render(strings.Join(lines, "\n"))
}

func (m *model) renderLogs() string {
	var title string
	if m.mode == viewLogs {
		title = headStyle.Render("LOGS") + dimStyle.Render("  "+m.cfg.container)
	} else {
		state := dimStyle.Render("no action run yet")
		switch {
		case m.busy:
			state = warnStyle.Render(spinner[m.ticks%len(spinner)] + " running")
		case m.actionLabel != "" && m.actionErr != nil:
			state = errStyle.Render("failed: " + runewidth.Truncate(m.actionErr.Error(), 40, "…"))
		case m.actionLabel != "":
			state = okStyle.Render("done")
		}
		title = headStyle.Render("ACTION") + "  " + boldStyle.Render(m.actionLabel) + "  " + state
	}
	if !m.follow {
		title += dimStyle.Render("  ·  paused, f to follow")
	}
	return boxStyle.Width(m.width - 2).Render(title + "\n" + m.vp.View())
}

func (m *model) renderHelp() string {
	pairs := [][2]string{
		{"s", "start"}, {"x", "stop"}, {"p", "pull"}, {"b", "build"}, {"u", "pull+build"},
		{"tab", "logs/action"}, {"↑↓", "scroll"}, {"f", "follow"}, {"q", "quit"},
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = keyStyle.Render(p[0]) + " " + dimStyle.Render(p[1])
	}
	left := " " + strings.Join(parts, "  ")

	right := m.result
	if m.busy {
		right = warnStyle.Render(spinner[m.ticks%len(spinner)] + " " + m.actionLabel + "…")
	}
	if right == "" {
		return left
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		return " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) View() string {
	if !m.ready {
		return "starting…"
	}
	return strings.Join([]string{
		m.renderBanner(),
		m.renderStatus(),
		m.renderJobs(),
		m.renderLogs(),
		m.renderHelp(),
	}, "\n")
}

// ────────────────────────────────── main ───────────────────────────────────

func main() {
	home, _ := os.UserHomeDir()
	cfg := config{}
	flag.StringVar(&cfg.dir, "dir", filepath.Join(home, "Projects", "bay-cron"), "bay-cron project directory")
	flag.StringVar(&cfg.container, "container", "bay-cron", "container name")
	flag.IntVar(&cfg.lines, "lines", 2000, "log lines to keep in view")
	flag.Parse()

	if st, err := os.Stat(cfg.dir); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "bay-cron directory not found: %s (use -dir)\n", cfg.dir)
		os.Exit(1)
	}

	logs := newLogStore(cfg.lines)
	f := &follower{}
	go f.loop(cfg.container, logs)

	m := &model{
		cfg:    cfg,
		logs:   logs,
		actBuf: &lineBuf{max: cfg.lines},
		follow: true,
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	f.kill()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}