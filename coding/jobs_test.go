package coding

import (
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
)

func jobsTools(t *testing.T) (bash, jobs *agent.Tool, m *jobManager) {
	t.Helper()
	m = newJobManager(t.TempDir())
	t.Cleanup(m.Close)
	opts := ToolOptions{jobs: m}
	return NewBashTool(t.TempDir(), opts), NewJobsTool(m), m
}

// alive reports whether a process exists, reaped or not.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitGone waits for pid to exit, which a process outside our own children
// does on its own schedule.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startedPid starts a job whose first line is the pid of a process it left
// running, and returns both.
func startedPid(t *testing.T, bash *agent.Tool, command string) (agent.ToolResult, int) {
	t.Helper()
	r, err := run(t, bash, map[string]any{"command": command, "description": "Starting a job", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(r.Text(), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			return r, pid
		}
	}
	t.Fatalf("no pid in:\n%s", r.Text())
	return r, 0
}

func TestBackgroundJobRunsReadsAndStops(t *testing.T) {
	bash, jobs, _ := jobsTools(t)
	r, pid := startedPid(t, bash, "sleep 300 & echo $!; echo ready; while :; do sleep 0.1; done")
	if !strings.Contains(r.Text(), "Started job 1") || !strings.Contains(r.Text(), "ready") {
		t.Fatalf("start:\n%s", r.Text())
	}
	if d := r.Details.(map[string]any); d["job"] != 1 || d["running"] != true {
		t.Fatalf("details: %v", d)
	}

	// Output already read is not read again.
	r, err := run(t, jobs, map[string]any{"action": "output", "id": float64(1)})
	if err != nil || !strings.Contains(r.Text(), "(no new output)") {
		t.Fatalf("second read: %v\n%s", err, r.Text())
	}
	r, _ = run(t, jobs, map[string]any{"action": "list"})
	if !strings.Contains(r.Text(), "job 1 · running") {
		t.Fatalf("list:\n%s", r.Text())
	}

	r, err = run(t, jobs, map[string]any{"action": "stop", "id": float64(1)})
	if err != nil || !strings.Contains(r.Text(), "job 1 · stopped") {
		t.Fatalf("stop: %v\n%s", err, r.Text())
	}
	// The process the job left running goes with it.
	waitGone(t, pid)
}

func TestReadingAJobWaitsForNewOutput(t *testing.T) {
	bash, jobs, _ := jobsTools(t)
	if _, err := run(t, bash, map[string]any{"command": "sleep 1; echo later; sleep 300", "description": "Waiting", "background": true}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r, err := run(t, jobs, map[string]any{"action": "output", "id": float64(1), "wait": float64(10)})
	if err != nil || !strings.Contains(r.Text(), "later") {
		t.Fatalf("waited read: %v\n%s", err, r.Text())
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("waited %v, past the output", waited)
	}
}

func TestAJobThatEndsAtOnceSaysSo(t *testing.T) {
	bash, jobs, _ := jobsTools(t)
	r, err := run(t, bash, map[string]any{"command": "echo no such thing >&2; exit 3", "description": "Failing", "background": true})
	if err != nil || !strings.Contains(r.Text(), "ended at once") || !strings.Contains(r.Text(), "exited with code 3") || !strings.Contains(r.Text(), "no such thing") {
		t.Fatalf("start:\n%v\n%s", err, r.Text())
	}
	if _, err := run(t, jobs, map[string]any{"action": "output", "id": float64(9)}); err == nil || !strings.Contains(err.Error(), "the jobs are 1") {
		t.Fatalf("unknown job: %v", err)
	}
}

func TestJobsAreLimited(t *testing.T) {
	bash, jobs, _ := jobsTools(t)
	for range MaxJobs {
		if _, err := run(t, bash, map[string]any{"command": "sleep 300", "description": "Sleeping", "background": true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, bash, map[string]any{"command": "sleep 300", "description": "Sleeping", "background": true}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("job past the limit: %v", err)
	}
	// Stopping one makes room.
	if _, err := run(t, jobs, map[string]any{"action": "stop", "id": float64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, bash, map[string]any{"command": "sleep 300", "description": "Sleeping", "background": true}); err != nil {
		t.Fatalf("after a stop: %v", err)
	}
}

// A job's output is kept to a bound; what is read past it says the rest is
// in the log.
func TestJobLogKeepsTheEnd(t *testing.T) {
	l, err := newJobLog(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	chunk := []byte(strings.Repeat("x", 1000) + "\n")
	for range 2 * jobLogKept / len(chunk) {
		l.Write(chunk)
	}
	out, dropped, end := l.since(0)
	if len(out) > jobLogKept || dropped == 0 || end != l.total || int64(len(out))+dropped != end {
		t.Fatalf("kept %d, dropped %d, end %d", len(out), dropped, end)
	}
	if out, dropped, _ := l.since(end); len(out) != 0 || dropped != 0 {
		t.Fatalf("read at the end: %d bytes, %d dropped", len(out), dropped)
	}
}

func TestBashOffersBackgroundOnlyWithJobs(t *testing.T) {
	if _, ok := NewBashTool(t.TempDir(), ToolOptions{}).Parameters["properties"].(map[string]any)["background"]; ok {
		t.Error("background offered without a place to keep jobs")
	}
	if tools := CodingTools(t.TempDir(), ToolOptions{}); toolNamed(tools, "jobs") {
		t.Error("jobs tool without jobs")
	}
}

func toolNamed(tools []*agent.Tool, name string) bool {
	for _, tl := range tools {
		if tl.Name == name {
			return true
		}
	}
	return false
}

// A session's jobs carry over to the session that replaces it, and end when
// the last one closes.
func TestSessionJobsCarryOverAndEndWithIt(t *testing.T) {
	s, _, _, _ := sandboxedSession(t, "off")
	if !toolNamed(s.currentTools(), "jobs") {
		t.Fatal("a session has no jobs tool")
	}
	j, err := s.jobs.start(s.Cwd(), ToolOptions{}, s.sandboxPolicy(), "sleep 300", "")
	if err != nil {
		t.Fatal(err)
	}
	pid := j.cmd.Process.Pid
	next, err := s.Reopen("")
	if err != nil {
		t.Fatal(err)
	}
	if !j.running() || next.jobs == nil || next.jobs.list()[0] != j {
		t.Fatal("the job did not carry over to the reopened session")
	}
	next.Close()
	waitGone(t, pid)
	if _, err := next.jobs.start(next.Cwd(), ToolOptions{}, next.sandboxPolicy(), "true", ""); err == nil {
		t.Fatal("started a job after the session closed")
	}
}
