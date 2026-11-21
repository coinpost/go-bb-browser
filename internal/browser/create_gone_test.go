package browser

import (
	"context"
	"testing"

	"github.com/chromedp/cdproto/target"
)

// Destructions are remembered only while a create is in flight, and reported
// for the target that create returned.
func TestDestroyedDuringCreateTracking(t *testing.T) {
	s := newSession(context.Background(), func() {})

	s.noteDestroyedDuringCreate("BEFORE")
	if len(s.destroyedInCreate) != 0 {
		t.Fatalf("remembered with no create in flight: %v", s.destroyedInCreate)
	}

	s.beginCreate()
	s.beginCreate()
	s.noteDestroyedDuringCreate("A")
	s.noteDestroyedDuringCreate("OTHER")
	if s.endCreate(target.ID("B")) {
		t.Fatal("create of B reported destroyed")
	}
	if !s.endCreate(target.ID("A")) {
		t.Fatal("create of A not reported destroyed")
	}
	if s.creating != 0 || len(s.destroyedInCreate) != 0 {
		t.Fatalf("tracking not released: creating=%d %v", s.creating, s.destroyedInCreate)
	}

	s.beginCreate()
	if s.endCreate("") {
		t.Fatal("failed create reported destroyed")
	}
}
