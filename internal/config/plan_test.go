package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadPlan_Basic(t *testing.T) {
	path := writeFile(t, "plan.yml", `- name: Foo::Bar
  title: 打招呼
  case_seq: 1
  counts: 2
  desc: test
  who: World
  n: 3
`)
	p, err := LoadPlan(path)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if len(p.Cases) != 1 {
		t.Fatalf("want 1 case, got %d", len(p.Cases))
	}
	c := p.Cases[0]
	if c.Name != "Foo::Bar" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Title != "打招呼" {
		t.Errorf("title = %q", c.Title)
	}
	if c.Desc != "test" {
		t.Errorf("desc = %q", c.Desc)
	}
	if c.CaseSeq != 1 {
		t.Errorf("case_seq = %d", c.CaseSeq)
	}
	if c.Counts != 2 {
		t.Errorf("counts = %d", c.Counts)
	}
	if v, ok := c.Args["who"].(string); !ok || v != "World" {
		t.Errorf("args[who] = %v (%T)", c.Args["who"], c.Args["who"])
	}
	// yaml.v3 decodes untyped integers as int
	if v, ok := c.Args["n"].(int); !ok || v != 3 {
		t.Errorf("args[n] = %v (%T)", c.Args["n"], c.Args["n"])
	}
}

func TestLoadPlan_DefaultCounts(t *testing.T) {
	path := writeFile(t, "plan.yml", "- name: A::B\n")
	p, err := LoadPlan(path)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if p.Cases[0].Counts != 1 {
		t.Errorf("default counts = %d", p.Cases[0].Counts)
	}
}

func TestLoadPlan_MissingName(t *testing.T) {
	path := writeFile(t, "plan.yml", "- title: oops\n")
	if _, err := LoadPlan(path); err == nil {
		t.Fatal("want error for missing name")
	}
}

func TestLoadPlan_NotSequence(t *testing.T) {
	path := writeFile(t, "plan.yml", "name: foo\n")
	if _, err := LoadPlan(path); err == nil {
		t.Fatal("want error for non-sequence top level")
	}
}

func TestLoadPlan_Empty(t *testing.T) {
	path := writeFile(t, "plan.yml", "[]\n")
	p, err := LoadPlan(path)
	if err != nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if len(p.Cases) != 0 {
		t.Errorf("want 0 cases, got %d", len(p.Cases))
	}
}

func TestLoadPlan_FileMissing(t *testing.T) {
	if _, err := LoadPlan("does-not-exist.yml"); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestLoadEnv(t *testing.T) {
	path := writeFile(t, "env.yml", "station: A1\noperator: 张三\nn: 42\n")
	env, err := LoadEnv(path)
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if env["station"] != "A1" {
		t.Errorf("station = %v", env["station"])
	}
	if env["operator"] != "张三" {
		t.Errorf("operator = %v", env["operator"])
	}
	// yaml.v3 decodes bare integers as int
	if env["n"] != 42 {
		t.Errorf("n = %v (%T)", env["n"], env["n"])
	}
}

func TestLoadEnv_FileMissing(t *testing.T) {
	if _, err := LoadEnv("does-not-exist.yml"); err == nil {
		t.Fatal("want error for missing file")
	}
}
