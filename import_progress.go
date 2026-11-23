// import_progress.go: what an import shows while it works, drawn with the
// shared kit (ui.go Progress and Steps), with numbers that are true.
//
//	Cleaning the conversation   bytes read out of the file, over its size
//	Uploading the conversation  bytes the service acknowledged, never bytes
//	                            written to the socket, so the bar does not run
//	                            ahead of what would survive a failure, and it
//	                            reaches 100% only when the service has
//	                            acknowledged the last byte
//	Placing on KeepState        a spinner (a step): nothing in it can be
//	                            measured from here, so there is no percentage
//
// With --json or --quiet nothing is drawn.
package main

// importBar is a kit bar that is off for --json and --quiet, and that can
// report what it was told (a test reads it).
type importBar struct {
	b    *bar
	seen *[]int64
}

func newImportBar(label string, total, already int64, seen *[]int64) *importBar {
	ib := &importBar{seen: seen}
	if out.json || out.quiet || total <= 0 {
		return ib
	}
	_, e := cmdUI()
	ib.b = e.Progress(label, total)
	if already > 0 { // a resume: what is already there is not this run's speed
		ib.b.done = already
		ib.b.samples = []barSample{{ib.b.u.now(), already}}
		ib.b.tenth = int(already * 10 / total)
	}
	return ib
}

// Set is told how many bytes are now done.
func (ib *importBar) Set(done int64) {
	if ib == nil {
		return
	}
	if ib.seen != nil {
		*ib.seen = append(*ib.seen, done)
	}
	if ib.b != nil {
		ib.b.Set(done)
	}
}

// Done replaces the bar by one line that stays on the screen.
func (ib *importBar) Done(label string) {
	if ib != nil && ib.b != nil {
		ib.b.Done(label)
	}
}

// DoneWith is Done with the result in the right-hand column, such as "537 MB
// down to 2.4 MB", in place of the size that was read.
func (ib *importBar) DoneWith(label, detail string) {
	if ib != nil && ib.b != nil {
		ib.b.DoneWith(label, detail)
	}
}

// importSteps is the kit's step list on stderr, or nil for --json and --quiet.
func importSteps() *stepList {
	if out.json || out.quiet {
		return nil
	}
	_, e := cmdUI()
	return e.Steps()
}
