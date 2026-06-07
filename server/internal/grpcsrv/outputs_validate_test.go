package grpcsrv

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateJobOutputs_Empty(t *testing.T) {
	if err := validateJobOutputs(nil); err != nil {
		t.Errorf("nil outputs should pass, got: %v", err)
	}
	if err := validateJobOutputs(map[string]string{}); err != nil {
		t.Errorf("empty outputs should pass, got: %v", err)
	}
}

func TestValidateJobOutputs_HappyPath(t *testing.T) {
	in := map[string]string{
		"next":         "v1.3.0",
		"kind":         "minor",
		"image-digest": "sha256:deadbeef",
	}
	if err := validateJobOutputs(in); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateJobOutputs_TooManyEntries(t *testing.T) {
	in := make(map[string]string, outputsServerMaxEntries+1)
	for i := 0; i <= outputsServerMaxEntries; i++ {
		in[fmt.Sprintf("k%d", i)] = "v"
	}
	err := validateJobOutputs(in)
	if err == nil {
		t.Fatal("expected entries-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "entries shipped") {
		t.Errorf("error should mention entries cap, got: %v", err)
	}
}

func TestValidateJobOutputs_BadAlias(t *testing.T) {
	cases := []struct {
		name  string
		alias string
	}{
		{"uppercase-leading", "Next"},
		{"digit-leading", "1next"},
		{"shell meta", "next; rm -rf /"},
		{"slash", "next/x"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateJobOutputs(map[string]string{tc.alias: "v"})
			if err == nil {
				t.Fatalf("expected bad-alias error for %q, got nil", tc.alias)
			}
			if !strings.Contains(err.Error(), "alias") {
				t.Errorf("error should mention 'alias', got: %v", err)
			}
		})
	}
}

func TestValidateJobOutputs_NonUTF8Value(t *testing.T) {
	// Invalid UTF-8 byte sequence — `\xff\xfe` is not the start of
	// any valid sequence. The proto wire is bytes-permissive so a
	// hostile client can ship this; the validator rejects to keep
	// the JSONB column textual.
	bad := string([]byte{0xff, 0xfe})
	err := validateJobOutputs(map[string]string{"next": bad})
	if err == nil {
		t.Fatal("expected UTF-8 rejection, got nil")
	}
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("error should mention UTF-8, got: %v", err)
	}
	// The error message must NOT echo the bad value — operators
	// debug from logs, and the value might be a token / digest /
	// secret accidentally.
	if strings.Contains(err.Error(), bad) {
		t.Error("error message leaked the non-UTF-8 value")
	}
}

func TestValidateJobOutputs_OverSizeCap(t *testing.T) {
	// One huge value pushes total over the cap.
	in := map[string]string{
		"big": strings.Repeat("x", outputsServerCapBytes+1),
	}
	err := validateJobOutputs(in)
	if err == nil {
		t.Fatal("expected size-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should mention cap, got: %v", err)
	}
}

func TestValidateJobOutputs_SizeCapCountsAllKeys(t *testing.T) {
	// 100 entries of ~700 bytes each totals ~70KB > 64KB cap.
	in := make(map[string]string, 100)
	val := strings.Repeat("x", 700)
	for i := 0; i < 100; i++ {
		in[fmt.Sprintf("a%d", i)] = val
	}
	err := validateJobOutputs(in)
	// EITHER entries-cap OR size-cap depending on iteration order;
	// both are valid rejections. The point is we don't accept.
	if err == nil {
		t.Fatal("expected rejection (entries OR size), got nil")
	}
}
