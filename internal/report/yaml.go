package report

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// YAMLReporter buffers CaseReports during a run and flushes a single YAML
// document on OnPlanStop. The top-level structure is a sequence of case
// entries (matching the Perl report style) followed by one summary block:
//
//	---
//	- case_seq: 1
//	  name: HelloSuite::SayHello
//	  result: ok
//	- summary:
//	    test_total_num: 1
//	    test_result: pass
//
// Output goes to an io.Writer (default os.Stdout) or, in file mode, to a
// report_<timestamp>.yml opened on OnPlanStart.
type YAMLReporter struct {
	w     io.Writer
	dir   string // non-empty => file mode
	f     *os.File
	cases []CaseReport
}

// NewYAML returns a reporter that writes to w. If w is nil, output goes to
// os.Stdout.
func NewYAML(w io.Writer) *YAMLReporter {
	if w == nil {
		w = os.Stdout
	}
	return &YAMLReporter{w: w}
}

// NewYAMLFile returns a reporter that opens report_<timestamp>.yml in dir
// when OnPlanStart is called. An empty dir means the current directory.
func NewYAMLFile(dir string) *YAMLReporter {
	return &YAMLReporter{dir: dir}
}

func (y *YAMLReporter) OnPlanStart(total int, startTime time.Time) {
	y.cases = y.cases[:0]
	if y.dir == "" {
		return
	}
	name := filepath.Join(y.dir, fmt.Sprintf("report_%s.yml", startTime.Format("20060102_150405")))
	f, err := os.Create(name)
	if err != nil {
		// Fall back to stdout rather than silently losing the report.
		y.w = os.Stdout
		return
	}
	y.f = f
	y.w = f
}

func (y *YAMLReporter) OnCaseStart(seq int, cr CaseReport) {}

func (y *YAMLReporter) OnCaseStop(seq int, cr CaseReport) {
	y.cases = append(y.cases, cr)
}

func (y *YAMLReporter) OnPlanStop(summary Summary) {
	defer y.closeFile()
	doc := &yaml.Node{Kind: yaml.DocumentNode}
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, cr := range y.cases {
		n := &yaml.Node{}
		if err := n.Encode(cr); err == nil {
			seq.Content = append(seq.Content, n)
		}
	}
	// Append the summary block as the last array element.
	sumNode := &yaml.Node{}
	if err := sumNode.Encode(map[string]Summary{"summary": summary}); err == nil {
		seq.Content = append(seq.Content, sumNode)
	}
	doc.Content = []*yaml.Node{seq}
	enc := yaml.NewEncoder(y.w)
	defer enc.Close()
	_ = enc.Encode(doc)
}

func (y *YAMLReporter) closeFile() {
	if y.f != nil {
		y.f.Close()
		y.f = nil
	}
}
