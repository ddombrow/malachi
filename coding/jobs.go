package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/sandbox"
)

// Background jobs are commands the model starts and does not wait for: a dev
// server, a watcher, a long build. The session keeps them, so their output
// can be read and they can be stopped later, and ends them when it ends:
// a command left running with "&" is otherwise nobody's, and outlives
// malachi.

const (
	// MaxJobs is how many background jobs may run at once in a session.
	MaxJobs = 8
	// jobLogKept is how much of a job's output stays in memory to be read.
	// All of it, up to MaxSpillBytes, is in the job's log file.
	jobLogKept = 1 << 20
	// maxJobWait caps how long reading a job's output may wait for more.
	maxJobWait = 30 * time.Second
	// jobStopGrace is how long a job has to exit after being asked to,
	// before it is killed.
	jobStopGrace = 3 * time.Second
)

// jobManager holds a session's background jobs.
type jobManager struct {
	mu     sync.Mutex
	next   int
	jobs   map[int]*job
	closed bool
	// dir holds the jobs' log files. It belongs to the manager, which a
	// reopened session takes over, and goes with it.
	dir string
	// life ends every job when the manager is closed.
	life   context.Context
	cancel context.CancelFunc
	// changed hears of every job starting and ending. The session that has
	// the manager binds it; a reopened session binds its own.
	changed func()
	// notifyMu makes each report wait for the one before it, so reports
	// arrive in the order the state they carry was read.
	notifyMu sync.Mutex
}

// bind sets who hears of jobs changing.
func (m *jobManager) bind(changed func()) {
	m.mu.Lock()
	m.changed = changed
	m.mu.Unlock()
}

// notify tells whoever is bound that a job changed.
func (m *jobManager) notify() {
	m.mu.Lock()
	changed := m.changed
	m.mu.Unlock()
	if changed != nil {
		m.notifyMu.Lock()
		defer m.notifyMu.Unlock()
		changed()
	}
}

func newJobManager(dir string) *jobManager {
	life, cancel := context.WithCancel(context.Background())
	return &jobManager{jobs: map[int]*job{}, dir: dir, life: life, cancel: cancel}
}

// job is one background command.
type job struct {
	id          int
	command     string
	description string
	started     time.Time
	cmd         *exec.Cmd
	log         *jobLog
	cancel      context.CancelFunc
	done        chan struct{}

	// Set once done is closed.
	exitCode int
	stopped  bool // ended by a stop, not on its own
	ended    time.Time

	mu     sync.Mutex
	readAt int64 // how much of the output has been read
}

// start runs command in the background under pol.
func (m *jobManager) start(cwd string, opts ToolOptions, pol sandbox.Policy, command, description string) (*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("the session is closing; no job can be started")
	}
	if n := m.runningLocked(); n >= MaxJobs {
		return nil, fmt.Errorf("%d jobs are already running, the most a session may have; stop one first (jobs action=stop)", n)
	}
	m.next++
	id := m.next
	log, err := newJobLog(m.dir, id)
	if err != nil {
		return nil, fmt.Errorf("cannot keep the job's output: %w", err)
	}
	ctx, cancel := context.WithCancel(m.life)
	cmd, err := sandbox.Command(ctx, pol, shellPath(), "-c", command)
	if err != nil {
		cancel()
		log.close()
		return nil, fmt.Errorf("%v. Commands cannot run until that is fixed or the user turns the sandbox off (-sandbox off)", err)
	}
	cmd.Dir = cwd
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = commandEnv(opts.AllowEnv, opts.HideEnv)
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		log.close()
		return nil, err
	}
	j := &job{id: id, command: command, description: description, started: time.Now(), cmd: cmd, log: log, cancel: cancel, done: make(chan struct{})}
	m.jobs[id] = j
	go func() {
		j.wait()
		m.notify()
	}()
	go m.notify() // m.mu is held here
	return j, nil
}

func (m *jobManager) runningLocked() int {
	n := 0
	for _, j := range m.jobs {
		if j.running() {
			n++
		}
	}
	return n
}

// wait records how the job ended.
func (j *job) wait() {
	_ = j.cmd.Wait()
	j.cancel()
	j.log.close()
	j.exitCode = -1
	if j.cmd.ProcessState != nil {
		j.exitCode = j.cmd.ProcessState.ExitCode()
	}
	j.ended = time.Now()
	close(j.done)
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// stop asks the job's processes to exit, and kills them if they have not
// within jobStopGrace.
func (j *job) stop() {
	if !j.running() {
		return
	}
	j.mu.Lock()
	j.stopped = true
	j.mu.Unlock()
	_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-j.done:
	case <-time.After(jobStopGrace):
		j.cancel() // kills the process group
		<-j.done
	}
}

// get returns job id, or an error naming the jobs there are.
func (m *jobManager) get(id int) (*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		return j, nil
	}
	if len(m.jobs) == 0 {
		return nil, fmt.Errorf("no job %d: this session has no jobs", id)
	}
	return nil, fmt.Errorf("no job %d: the jobs are %s", id, strings.Join(m.idsLocked(), ", "))
}

func (m *jobManager) idsLocked() []string {
	var ids []string
	for _, id := range m.sortedLocked() {
		ids = append(ids, fmt.Sprint(id))
	}
	return ids
}

func (m *jobManager) sortedLocked() []int {
	ids := make([]int, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// list returns the jobs in the order they were started.
func (m *jobManager) list() []*job {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*job
	for _, id := range m.sortedLocked() {
		out = append(out, m.jobs[id])
	}
	return out
}

// Close stops every job and removes their logs. Nothing can be started after.
func (m *jobManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	jobs := make([]*job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Go(j.stop)
	}
	wg.Wait()
	m.cancel()
	if m.dir != "" {
		_ = os.RemoveAll(m.dir)
	}
}

// status says how the job is doing, in a few words.
func (j *job) status() string {
	if j.running() {
		return "running for " + time.Since(j.started).Truncate(time.Second).String()
	}
	j.mu.Lock()
	stopped := j.stopped
	j.mu.Unlock()
	switch {
	case stopped:
		return "stopped"
	case j.exitCode == 0:
		return "exited"
	case j.exitCode < 0:
		return "ended by a signal"
	}
	return fmt.Sprintf("exited with code %d", j.exitCode)
}

// jobLog keeps a job's output: the last jobLogKept bytes in memory, by
// their offsets in the whole stream, and all of it (up to MaxSpillBytes)
// in a file.
type jobLog struct {
	mu      sync.Mutex
	buf     []byte
	start   int64 // offset of buf[0] in the whole output
	total   int64
	file    *os.File
	path    string
	written int64
	changed chan struct{} // closed and replaced on each write
}

func newJobLog(dir string, id int) (*jobLog, error) {
	f, err := createSpillNamed(dir, fmt.Sprintf("malachi-job-%d-*.log", id))
	if err != nil {
		return nil, err
	}
	return &jobLog{file: f, path: f.Name(), changed: make(chan struct{})}, nil
}

func (l *jobLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total += int64(len(p))
	l.buf = append(l.buf, p...)
	if extra := len(l.buf) - jobLogKept; extra > 0 {
		l.buf = append(l.buf[:0], l.buf[extra:]...)
		l.start += int64(extra)
	}
	if l.file != nil && l.written < MaxSpillBytes {
		n, _ := l.file.Write(p[:min(int64(len(p)), MaxSpillBytes-l.written)])
		l.written += int64(n)
	}
	close(l.changed)
	l.changed = make(chan struct{})
	return len(p), nil
}

// since returns the output from offset on, how much of it before that was
// no longer kept, and the offset it ends at.
func (l *jobLog) since(offset int64) (out []byte, dropped, end int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if offset < l.start {
		dropped, offset = l.start-offset, l.start
	}
	return slices.Clone(l.buf[offset-l.start:]), dropped, l.total
}

// waitFor returns a channel closed by the next write.
func (l *jobLog) waitFor() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

func (l *jobLog) size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

func (l *jobLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// read returns what the job has printed since it was last read, waiting up
// to wait for something new while it runs.
func (j *job) read(ctx context.Context, wait time.Duration) (text string, dropped int64) {
	j.mu.Lock()
	from := j.readAt
	j.mu.Unlock()
	if wait > 0 && j.log.size() == from && j.running() {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-j.log.waitFor():
			// Let a burst of output arrive whole.
			time.Sleep(100 * time.Millisecond)
		case <-j.done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	out, dropped, end := j.log.since(from)
	j.mu.Lock()
	j.readAt = end
	j.mu.Unlock()
	return string(out), dropped
}

// NewJobsTool returns the tool that reads and stops the background jobs bash
// starts.
func NewJobsTool(jobs *jobManager) *agent.Tool {
	return &agent.Tool{
		Name:  "jobs",
		Label: "Jobs",
		Description: "List, read and stop background jobs, which bash starts with background=true. " +
			"action=list shows every job and its state. action=output returns what a job has printed since it was last read, " +
			fmt.Sprintf("waiting up to wait seconds (at most %d) for something new while it runs. ", int(maxJobWait.Seconds())) +
			"action=stop ends a job and returns its last output. Jobs end when the session does.",
		PromptSnippet: "Read and stop background jobs",
		PromptGuidelines: []string{
			"Start a server, watcher or anything else that does not exit with bash background=true, then use jobs to read its output and stop it, instead of ending the command with &.",
			"Stop a background job once it is no longer needed.",
		},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"list", "output", "stop"}, "description": "list, output or stop"},
				"id":     map[string]any{"type": "integer", "description": "The job, for output and stop"},
				"wait":   map[string]any{"type": "number", "description": fmt.Sprintf("For output: seconds to wait for new output while the job runs (default 0, at most %d)", int(maxJobWait.Seconds()))},
			},
			"required": []string{"action"},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeJobs(ctx, jobs, args)
		},
	}
}

func executeJobs(ctx context.Context, jobs *jobManager, args map[string]any) (agent.ToolResult, error) {
	action, err := strArg(args, "action")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if action == "list" {
		return listJobs(jobs), nil
	}
	if action != "output" && action != "stop" {
		return agent.ToolResult{}, fmt.Errorf("action must be list, output or stop, not %q", action)
	}
	id, ok, err := optInt(args, "id")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !ok {
		return agent.ToolResult{}, fmt.Errorf("action %s needs the job's id", action)
	}
	j, err := jobs.get(id)
	if err != nil {
		return agent.ToolResult{}, err
	}
	var wait time.Duration
	if action == "output" {
		secs, _, err := optNumber(args, "wait")
		if err != nil {
			return agent.ToolResult{}, err
		}
		wait = min(time.Duration(max(secs, 0)*float64(time.Second)), maxJobWait)
	} else {
		j.stop()
	}
	out, dropped := j.read(ctx, wait)
	return jobResult(j, out, dropped), nil
}

func listJobs(jobs *jobManager) agent.ToolResult {
	all := jobs.list()
	if len(all) == 0 {
		return agent.TextResult("No jobs. Start one with bash background=true.")
	}
	var b strings.Builder
	var details []map[string]any
	for _, j := range all {
		fmt.Fprintf(&b, "job %d · %s · $ %s\n", j.id, j.status(), jobCommandLine(j.command))
		details = append(details, jobDetails(j))
	}
	return agent.ToolResult{Content: []agent.Content{&agent.TextContent{Text: strings.TrimRight(b.String(), "\n")}}, Details: map[string]any{"jobs": details}}
}

func jobResult(j *job, out string, dropped int64) agent.ToolResult {
	head := fmt.Sprintf("job %d · %s · $ %s", j.id, j.status(), jobCommandLine(j.command))
	t := TruncateTail(out, MaxOutputLines, MaxOutputBytes)
	body := t.Content
	switch {
	case out == "":
		body = "(no new output)"
	case dropped > 0 || t.Truncated:
		body = fmt.Sprintf("[Earlier output not shown. The whole log: %s]\n", j.log.path) + body
	}
	d := jobDetails(j)
	d["output_bytes"], d["dropped_bytes"] = len(out), dropped
	return agent.ToolResult{Content: []agent.Content{&agent.TextContent{Text: head + "\n\n" + body}}, Details: d}
}

func jobDetails(j *job) map[string]any {
	d := map[string]any{"job": j.id, "command": j.command, "status": j.status(), "running": j.running(), "log": j.log.path}
	if !j.running() {
		d["exit_code"] = j.exitCode
	}
	return d
}

func jobCommandLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// JobState is one background job as frontends show it.
type JobState struct {
	ID          int
	Command     string
	Description string
	// Status is running, exited or stopped.
	Status   string
	ExitCode int // once exited
	Started  time.Time
	// Log is the file holding the job's whole output.
	Log string
}

// Job statuses.
const (
	JobRunning = "running"
	JobExited  = "exited"
	JobStopped = "stopped"
)

func (j *job) state() JobState {
	st := JobState{ID: j.id, Command: j.command, Description: j.description, Started: j.started, Log: j.log.path, Status: JobRunning}
	if !j.running() {
		j.mu.Lock()
		stopped := j.stopped
		j.mu.Unlock()
		st.Status, st.ExitCode = JobExited, j.exitCode
		if stopped {
			st.Status = JobStopped
		}
	}
	return st
}

// states returns every job's state, in the order they were started.
func (m *jobManager) states() []JobState {
	var out []JobState
	for _, j := range m.list() {
		out = append(out, j.state())
	}
	return out
}
