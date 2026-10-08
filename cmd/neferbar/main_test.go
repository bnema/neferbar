package main

import (
	"bytes"
	"testing"
)

func TestPrintVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "1.2.3"

	var out bytes.Buffer
	if err := printVersion(nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "1.2.3\n" {
		t.Fatalf("got %q, want %q", got, "1.2.3\n")
	}
	if err := printVersion([]string{"extra"}, &out); err == nil {
		t.Fatal("extra argument accepted")
	}
}
