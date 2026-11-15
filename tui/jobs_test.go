package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/coding"
)

func TestSidebarShowsJobs(t *testing.T) {
	m := newTestModel(t)
	if strings.Contains(sidebarText(m), "jobs") {
		t.Fatalf("a jobs section without jobs:\n%s", sidebarText(m))
	}
	now := time.Now()
	m.handleEvent(coding.JobsChangedEvent{State: []coding.JobState{
		{ID: 1, Command: "npm run dev", Description: "Serving the app", Status: coding.JobRunning, Started: now},
		{ID: 2, Command: "go test ./...", Status: coding.JobExited, ExitCode: 1, Started: now},
		{ID: 3, Command: "tail -f log", Status: coding.JobStopped, Started: now},
	}})
	text := ansi.Strip(strings.Join(m.sidebarLines(60), "\n"))
	for _, want := range []string{"jobs", "1 running", "1 Serving the app", "running", "2 $ go test", "exit 1", "3 $ tail -f log", "stopped"} {
		if !strings.Contains(text, want) {
			t.Errorf("sidebar lacks %q:\n%s", want, text)
		}
	}
}

// Running jobs are always listed; ended ones fill what room is left, newest
// first, and the rest are counted.
func TestSidebarListsRunningJobsFirst(t *testing.T) {
	m := newTestModel(t)
	var jobs []coding.JobState
	for i := 1; i <= 9; i++ {
		jobs = append(jobs, coding.JobState{ID: i, Command: fmt.Sprintf("cmd%d", i), Status: coding.JobExited})
	}
	jobs[1].Status = coding.JobRunning // job 2
	m.handleEvent(coding.JobsChangedEvent{State: jobs})
	text := ansi.Strip(strings.Join(m.sidebarLines(60), "\n"))
	if !strings.Contains(text, "2 $ cmd2") || !strings.Contains(text, "9 $ cmd9") || strings.Contains(text, "1 $ cmd1") {
		t.Errorf("wrong jobs listed:\n%s", text)
	}
	if !strings.Contains(text, fmt.Sprintf("%d more ended", 9-maxSidebarJobs)) {
		t.Errorf("hidden jobs not counted:\n%s", text)
	}
}
