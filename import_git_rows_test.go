package main

import (
	"strings"
	"testing"
)

// The rows of a choice wrap on word boundaries inside the width, so a narrow
// terminal never scrolls the list out from under its redraw.
func TestChoiceRowsWrapInsideTheWidth(t *testing.T) {
	items := []igItem{{Text: "conversation only 2.4 MB"}, {Text: "with the files and their history (1.8 GB, over the 1 GB limit)", Off: true}, {Text: "with the files only, without the history (214 MB, 3,120 files)"}, {Text: "cancel"}}
	for _, w := range []int{120, 80, 60, 40} {
		rows := igRows(items, 2, w, func(s string) string { return s })
		for _, r := range rows {
			if displayWidth(r) > w {
				t.Errorf("%d columns: %q is %d wide", w, r, displayWidth(r))
			}
		}
		text := strings.Join(rows, "\n")
		if w >= 80 && len(rows) != 4 {
			t.Errorf("%d columns: %d rows, want one a choice:\n%s", w, len(rows), text)
		}
		if strings.Contains(strings.Join(strings.Fields(text), " "), "1 GB li mit") || !strings.Contains(flat(text), "over the 1 GB limit") {
			t.Errorf("%d columns: a word was cut:\n%s", w, text)
		}
	}
}
