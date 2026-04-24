package runner

import (
	"os"
	"path/filepath"
	"testing"
)

const junitAggregate = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="pkg.a">
    <testcase classname="pkg.a.FooTest" name="passes" time="0.1"/>
    <testcase classname="pkg.a.FooTest" name="breaks" time="0.2">
      <failure type="AssertionError" message="want 1 got 2">stack here</failure>
    </testcase>
    <testcase classname="pkg.a.FooTest" name="erred">
      <error type="IOError" message="boom"/>
    </testcase>
    <testcase classname="pkg.a.FooTest" name="skipped">
      <skipped message="todo"/>
    </testcase>
  </testsuite>
  <testsuite name="pkg.b">
    <testcase classname="pkg.b.Bar" name="ok" time="0"/>
  </testsuite>
</testsuites>`

const junitSingle = `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="solo">
  <testcase classname="solo.T" name="one" time="0.5"/>
</testsuite>`

func TestDecodeJUnit_AggregateRoot(t *testing.T) {
	suites, err := decodeJUnit([]byte(junitAggregate))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(suites) != 2 {
		t.Fatalf("suites = %d", len(suites))
	}
	if got := suites[0].Cases; len(got) != 4 {
		t.Errorf("suite[0] cases = %d", len(got))
	}
}

func TestDecodeJUnit_SingleRoot(t *testing.T) {
	suites, err := decodeJUnit([]byte(junitSingle))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(suites) != 1 || suites[0].Name != "solo" {
		t.Fatalf("suites = %+v", suites)
	}
}

func TestCaseToProto_StatusMapping(t *testing.T) {
	suites, _ := decodeJUnit([]byte(junitAggregate))
	byName := map[string]string{}
	for _, s := range suites {
		for _, c := range s.Cases {
			p := caseToProto(s.Name, c)
			byName[c.Name] = p.GetStatus()
		}
	}
	want := map[string]string{
		"passes":  "passed",
		"breaks":  "failed",
		"erred":   "errored",
		"skipped": "skipped",
		"ok":      "passed",
	}
	for name, w := range want {
		if got := byName[name]; got != w {
			t.Errorf("%s status = %q, want %q", name, got, w)
		}
	}
}

func TestCaseToProto_DurationMilliseconds(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0.1", 100},
		{"0.5", 500},
		{"0", 0},
		{"", 0},
		{"nope", 0},
	}
	for _, c := range cases {
		got := parseDurationSeconds(c.in)
		if got != c.want {
			t.Errorf("parseDurationSeconds(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseJUnitFiles_AggregatesAcrossFiles(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a.xml")
	b := filepath.Join(tmp, "b.xml")
	if err := os.WriteFile(a, []byte(junitAggregate), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(junitSingle), 0o644); err != nil {
		t.Fatal(err)
	}
	// A third file that's bogus to exercise the warning path.
	bad := filepath.Join(tmp, "c.xml")
	if err := os.WriteFile(bad, []byte("<nope/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, warnings := parseJUnitFiles([]string{a, b, bad})
	if len(results) != 6 {
		t.Errorf("results = %d, want 6", len(results))
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %d, want 1 (bad root)", len(warnings))
	}
}

func TestExpandGlobs_DropsDirsAndDupes(t *testing.T) {
	tmp := t.TempDir()
	// Two .xml files and one directory named like a glob target.
	for _, p := range []string{"x.xml", "y.xml"} {
		if err := os.WriteFile(filepath.Join(tmp, p), []byte("<testsuite name='t'/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(tmp, "dir.xml"), 0o755); err != nil {
		t.Fatal(err)
	}

	matches, err := expandGlobs(tmp, []string{"*.xml", "*.xml"}) // dupe on purpose
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(matches) != 2 {
		t.Errorf("matches = %d, want 2", len(matches))
	}
}
