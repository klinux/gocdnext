package runner

import (
	"strings"
	"testing"
)

const goCoverFixture = `mode: set
github.com/acme/shop/internal/cart/cart.go:10.20,12.2 2 1
github.com/acme/shop/internal/cart/cart.go:14.2,16.10 3 0
github.com/acme/shop/internal/pay/pay.go:5.1,9.2 4 1
github.com/acme/shop/internal/pay/pay.go:11.1,12.2 1 1
`

const lcovFixture = `TN:
SF:src/app/page.tsx
DA:1,1
DA:2,0
DA:3,4
LF:3
LH:2
end_of_record
SF:src/lib/util.ts
DA:1,1
LF:1
LH:1
end_of_record
`

const coberturaFixture = `<?xml version="1.0"?>
<coverage lines-valid="10" lines-covered="7" version="1.9">
  <packages>
    <package name="com.acme.cart">
      <classes>
        <class filename="Cart.java">
          <lines>
            <line number="1" hits="1"/>
            <line number="2" hits="0"/>
            <line number="3" hits="5"/>
          </lines>
        </class>
      </classes>
    </package>
    <package name="com.acme.pay">
      <classes>
        <class filename="Pay.java">
          <lines>
            <line number="1" hits="1"/>
            <line number="2" hits="1"/>
            <line number="3" hits="0"/>
            <line number="4" hits="0"/>
            <line number="5" hits="2"/>
            <line number="6" hits="1"/>
            <line number="7" hits="0"/>
          </lines>
        </class>
      </classes>
    </package>
  </packages>
</coverage>
`

func TestParseCoverage(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		data        string
		wantCovered int64
		wantTotal   int64
		wantPkgs    int
	}{
		// go-cover counts STATEMENTS: covered 2+4+1=7 of 2+3+4+1=10.
		{"go-cover", "go-cover", goCoverFixture, 7, 10, 2},
		// lcov: LH/LF per record: 2/3 + 1/1 = 3/4.
		{"lcov", "lcov", lcovFixture, 3, 4, 2},
		// cobertura: summed from <line> elements: (2/3)+(4/7) = 6/10.
		{"cobertura", "cobertura", coberturaFixture, 6, 10, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum, warns, err := parseCoverage(tt.format, []byte(tt.data))
			if err != nil {
				t.Fatalf("parse: %v (warns=%v)", err, warns)
			}
			if sum.LinesCovered != tt.wantCovered || sum.LinesTotal != tt.wantTotal {
				t.Fatalf("covered/total = %d/%d, want %d/%d",
					sum.LinesCovered, sum.LinesTotal, tt.wantCovered, tt.wantTotal)
			}
			if len(sum.Packages) != tt.wantPkgs {
				t.Fatalf("packages = %d, want %d (%+v)", len(sum.Packages), tt.wantPkgs, sum.Packages)
			}
		})
	}
}

func TestParseCoverage_GoCoverDuplicateBlocks(t *testing.T) {
	// `go test ./... -coverprofile` can repeat a block (one line per
	// package run, atomic mode). The block counts once; covered
	// when ANY occurrence has count > 0.
	data := `mode: atomic
pkg/a/a.go:1.1,2.2 3 0
pkg/a/a.go:1.1,2.2 3 5
`
	sum, _, err := parseCoverage("go-cover", []byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.LinesTotal != 3 || sum.LinesCovered != 3 {
		t.Fatalf("dup blocks must merge: %d/%d, want 3/3", sum.LinesCovered, sum.LinesTotal)
	}
}

func TestParseCoverage_Malformed(t *testing.T) {
	for _, tt := range []struct{ name, format, data string }{
		{"go-cover garbage", "go-cover", "not a profile"},
		{"go-cover empty", "go-cover", "mode: set\n"},
		{"lcov empty", "lcov", "TN:\n"},
		{"cobertura not xml", "cobertura", "{json?}"},
		{"unknown format", "gcov", "anything"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseCoverage(tt.format, []byte(tt.data)); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestParseCoverage_PackageCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("mode: set\n")
	for i := 0; i < maxCoveragePackages+50; i++ {
		b.WriteString("pkg/p")
		for j := 0; j < 3; j++ {
			b.WriteByte(byte('a' + (i/(j+1))%26))
		}
		b.WriteString("/f.go:1.1,2.2 1 1\n")
	}
	sum, warns, err := parseCoverage("go-cover", []byte(b.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sum.Packages) > maxCoveragePackages {
		t.Fatalf("packages = %d, want <= %d", len(sum.Packages), maxCoveragePackages)
	}
	// The cap must be LOUD (no silent truncation).
	found := false
	for _, w := range warns {
		if strings.Contains(w, "package breakdown truncated") {
			found = true
		}
	}
	if !found && len(sum.Packages) == maxCoveragePackages {
		t.Fatalf("expected truncation warning, warns=%v", warns)
	}
}

// Review-round LOW: malformed LF/LH must reject the record loudly —
// silently skipping them persisted >100% packages.
func TestParseCoverage_LCOVStrictness(t *testing.T) {
	tests := []struct{ name, data string }{
		{"LH exceeds LF", "SF:a.ts\nLF:2\nLH:5\nend_of_record\n"},
		{"negative LF", "SF:a.ts\nLF:-1\nLH:0\nend_of_record\n"},
		{"unparsable LF", "SF:a.ts\nLF:abc\nLH:1\nend_of_record\n"},
		{"unparsable LH", "SF:a.ts\nLF:3\nLH:x\nend_of_record\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseCoverage("lcov", []byte(tt.data)); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

// Phase 2: the fail_under gate. At-threshold passes (>= contract);
// zero means "no gate"; below fails with both numbers in the reason.
func TestCoverageGate(t *testing.T) {
	tests := []struct {
		name      string
		failUnder float64
		covered   int64
		total     int64
		wantFail  bool
	}{
		{"no gate", 0, 1, 100, false},
		{"below threshold fails", 80, 70, 100, true},
		{"at threshold passes", 70, 70, 100, false},
		{"above passes", 50, 70, 100, false},
		{"empty total with gate fails", 80, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failed, reason := coverageGate(tt.failUnder, tt.covered, tt.total)
			if failed != tt.wantFail {
				t.Fatalf("gate = %v (%s), want %v", failed, reason, tt.wantFail)
			}
			if failed && !strings.Contains(reason, "fail_under") {
				t.Fatalf("reason should cite the knob: %q", reason)
			}
		})
	}
}
