package runner

import (
	"strings"
	"testing"
)

// TestTheBufferDropsTheOLDESTRatherThanBlockingTheAgent — A DROPPED DIAGNOSTIC IS CHEAPER THAN A
// STOPPED MACHINE.
//
// This buffer sits under the agent's own log output. A bounded buffer that blocked its writer would
// stop the machine doing its work in order to complain about it, and the machines this runs on are the
// ones nobody is watching. So the newest line always wins and the oldest is what goes.
func TestTheBufferDropsTheOLDESTRatherThanBlockingTheAgent(t *testing.T) {
	b := NewLogBuffer(3)
	for _, line := range []string{"one", "two", "three", "four"} {
		if _, err := b.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}
	got := b.Take(10)
	if len(got) != 3 {
		t.Fatalf("buffer held %d lines, want its capacity of 3", len(got))
	}
	if got[0].Message != "two" || got[2].Message != "four" {
		t.Fatalf("buffer kept %q..%q, want the NEWEST three (two..four) — dropping the newest would lose "+
			"the line that describes what just went wrong", got[0].Message, got[2].Message)
	}
	// Taken lines are gone: a second shipment must not resend what the first one carried.
	if again := b.Take(10); len(again) != 0 {
		t.Fatalf("Take left %d line(s) behind — every shipment would resend them", len(again))
	}
}

// TestABlankLineIsNotAShipment keeps the newline the logger adds, and anything that is only whitespace,
// out of a fleet's log table. It is the cheapest possible filter and it runs on the machine, which is
// the only place it costs nothing.
func TestABlankLineIsNotAShipment(t *testing.T) {
	b := NewLogBuffer(10)
	for _, blank := range []string{"\n", "   \n", "\t\n", ""} {
		if _, err := b.Write([]byte(blank)); err != nil {
			t.Fatalf("write %q: %v", blank, err)
		}
	}
	if got := b.Take(10); len(got) != 0 {
		t.Fatalf("%d blank line(s) were buffered for shipment", len(got))
	}
}

// TestTheTrailingNewlineIsTrimmedOnce — the standard logger appends one, and a message stored with it
// renders as a blank line in every reader downstream.
func TestTheTrailingNewlineIsTrimmedOnce(t *testing.T) {
	b := NewLogBuffer(10)
	if _, err := b.Write([]byte("runner gateway listening on :8443\n")); err != nil {
		t.Fatal(err)
	}
	got := b.Take(1)
	if len(got) != 1 {
		t.Fatal("the line was not buffered")
	}
	if strings.HasSuffix(got[0].Message, "\n") {
		t.Fatalf("message keeps its trailing newline: %q", got[0].Message)
	}
	if got[0].At.IsZero() {
		t.Fatal("the line carries no timestamp — the plane could not order it against the machine's own clock")
	}
}
