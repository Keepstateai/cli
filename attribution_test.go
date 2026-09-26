// KS-049: transcript entries render by the control plane's attribution:
// an instruction with its task and author, a consultation with the asking
// agent, advice with the adviser; an entry with no author is unattributed,
// never the person's.
package main

import (
	"strings"
	"testing"
)

func TestTranscriptEntriesRenderByAttribution(t *testing.T) {
	cases := []struct {
		e    transcriptEntry
		want []string
		not  []string
	}{
		{transcriptEntry{Kind: "instruction", Text: "run the tests", TaskID: "tsk_1", AuthorType: "account", AuthorID: "acct_1"},
			[]string{"asked", "account acct_1", "task tsk_1", "run the tests"}, nil},
		{transcriptEntry{Kind: "instruction", Text: "do it", TaskID: "tsk_2"},
			[]string{"unattributed", "task tsk_2"}, []string{"account"}},
		{transcriptEntry{Kind: "consultation", Text: "is this safe?", ConsultationID: "cons_1", AuthorType: "agent", AuthorID: "agent_src"},
			[]string{"question from agent agent_src", "cons_1"}, []string{"asked "}},
		{transcriptEntry{Kind: "advice", Text: "looks fine", ConsultationID: "cons_1", AuthorType: "adviser", AuthorID: "agent_rev"},
			[]string{"advice", "from adviser agent_rev"}, nil},
		{transcriptEntry{Kind: "advice", Text: "?", ConsultationID: "cons_2"},
			[]string{"unattributed"}, []string{"from "}},
	}
	for _, c := range cases {
		got := transcriptLine(c.e)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.e.Kind, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q contains %q", c.e.Kind, got, n)
			}
		}
	}
}
