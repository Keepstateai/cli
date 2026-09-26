// BACKLOG-130: both models are shown as recorded; a difference is said, and
// an absent one reads not recorded, never inferred.
package main

import (
	"strings"
	"testing"
)

func TestRunnerModelLineShowsBoth(t *testing.T) {
	if l := runnerModelLine(agentRow{RunnerModel: "claude-a", RunnerReportedModel: "claude-a"}); !strings.Contains(l, "started with claude-a; the runner reported claude-a") || strings.Contains(l, "DIFFER") {
		t.Errorf("same: %q", l)
	}
	if l := runnerModelLine(agentRow{RunnerModel: "claude-a", RunnerReportedModel: "claude-b"}); !strings.Contains(l, "THESE DIFFER") {
		t.Errorf("differ: %q", l)
	}
	if l := runnerModelLine(agentRow{}); strings.Count(l, "not recorded") != 2 {
		t.Errorf("absent: %q", l)
	}
}
