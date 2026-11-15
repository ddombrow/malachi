package tui

import (
	"fmt"

	"charm.land/lipgloss/v2"

	"github.com/ddombrow/malachi/coding"
)

// maxSidebarJobs is how many jobs the sidebar lists: the running ones first,
// then the most recently ended.
const maxSidebarJobs = 6

func (m *model) syncJobs(jobs []coding.JobState) {
	m.side.jobs = jobs
}

// jobsSection lists the session's background jobs, or nothing when there
// are none.
func (m *model) jobsSection() []sideRow {
	jobs, s := m.side.jobs, m.r.st
	if len(jobs) == 0 {
		return nil
	}
	var running, ended []coding.JobState
	for _, j := range jobs {
		if j.Status == coding.JobRunning {
			running = append(running, j)
		} else {
			ended = append(ended, j)
		}
	}
	var rows []sideRow
	add := func(text string) { rows = append(rows, sideRow{text: text}) }
	add(sidePair(s.accent.Render("jobs"), s.dim.Render(fmt.Sprintf("%d running", len(running)))))
	// The newest ended jobs, after every running one, as room allows.
	shown := running
	for i := len(ended) - 1; i >= 0 && len(shown) < maxSidebarJobs; i-- {
		shown = append(shown, ended[i])
	}
	for _, j := range shown {
		status := jobStatus(s, j)
		name := truncateEnd(fmt.Sprintf("  %d %s", j.ID, jobLabel(j)), sidebarInner-lipgloss.Width(status)-1)
		add(sidePair(name, status))
	}
	if hidden := len(jobs) - len(shown); hidden > 0 {
		add(s.dim.Render(fmt.Sprintf("  %d more ended", hidden)))
	}
	return rows
}

// jobLabel names a job: its description, or failing that its command.
func jobLabel(j coding.JobState) string {
	if j.Description != "" {
		return j.Description
	}
	return "$ " + j.Command
}

// jobStatus is a job's state as one short styled word.
func jobStatus(st styles, j coding.JobState) string {
	switch {
	case j.Status == coding.JobRunning:
		return st.toolRun.Render("running")
	case j.Status == coding.JobStopped:
		return st.dim.Render("stopped")
	case j.ExitCode == 0:
		return st.toolOK.Render("exit 0")
	case j.ExitCode < 0:
		return st.toolErr.Render("killed")
	}
	return st.toolErr.Render(fmt.Sprintf("exit %d", j.ExitCode))
}
