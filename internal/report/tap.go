package report

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// TAPReporter emits TAP version 13 output
// (https://testanything.org/tap-version-13-specification.html):
//
//	TAP version 13
//	1..N
//	ok 1 - HelloSuite::SayHello
//	not ok 2 - SomeCase
//	  ---
//	  message: 'expected X, got Y'
//	  ...
//
// Failures carry a TAP diagnostic YAML block with the error message.
// OnPlanStop emits a "Bail out!" line when the plan contains failures.
type TAPReporter struct {
	w       io.Writer
	total   int
	started bool
}

// NewTAP returns a reporter writing to w. If w is nil, output goes to
// os.Stdout.
func NewTAP(w io.Writer) *TAPReporter {
	if w == nil {
		w = os.Stdout
	}
	return &TAPReporter{w: w}
}

func (t *TAPReporter) OnPlanStart(total int, startTime time.Time) {
	t.total = total
	t.started = true
	// fmt.Fprintf(t.w, "TAP version 13\n1..%d\n", total)
}

func (t *TAPReporter) OnCaseStart(seq int, cr CaseReport) {}

func (t *TAPReporter) OnCaseStop(seq int, cr CaseReport) {
	title := cr.Title
	if title == "" {
		title = cr.Name
	}
	if cr.Result == CaseFail {
		fmt.Fprintf(t.w, "not ok %d - %s\n", seq, tapEscape(title))
		if cr.Error != "" {
			t.writeDiagnostic(cr.Error)
		}
		return
	}
	fmt.Fprintf(t.w, "ok %d - %s\n", seq, tapEscape(title))
}

func (t *TAPReporter) OnPlanStop(summary Summary) {
	if summary.FailNum > 0 {
		fmt.Fprintf(t.w, "Bail out! %d case(s) failed\n", summary.FailNum)
	}
}

// writeDiagnostic emits a TAP-13 diagnostic YAML block, indented two
// spaces per the specification.
func (t *TAPReporter) writeDiagnostic(msg string) {
	msg = strings.ReplaceAll(msg, "'", "''")
	fmt.Fprintf(t.w, "  ---\n  message: '%s'\n  ...\n", msg)
}

// tapEscape neutralises '#' which terminates a TAP description early.
func tapEscape(s string) string {
	return strings.ReplaceAll(s, "#", "\\#")
}
