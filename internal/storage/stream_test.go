package storage

import (
	"errors"
	"testing"
)

func TestUnknownReservationsShareBudget(t *testing.T) {
	s, _, _ := openStore(t, 192<<20)
	first, err := s.Reserve(0)
	if err != nil {
		t.Fatal(err)
	}

	second, err := s.Reserve(0)
	if err != nil {
		t.Fatal("first unknown-size download reserved the entire budget:", err)
	}

	if err := first.Grow(120 << 20); err != nil {
		t.Fatal(err)
	}
	if err := second.Grow(100 << 20); !errors.Is(err, ErrCapacity) {
		t.Fatal("growth overcommitted the shared budget:", err)
	}

	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}

	if err := second.Grow(100 << 20); err != nil {
		t.Fatal("aborted reservation did not release its expanded capacity:", err)
	}

	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := second.Grow(100 << 20); err == nil {
		t.Fatal("cleared download continued reserving space")
	}
	if err := second.Abort(); err != nil {
		t.Fatal(err)
	}

	last, err := s.Reserve(192 << 20)
	if err != nil {
		t.Fatal("growth leaked reserved capacity:", err)
	}
	if err := last.Abort(); err != nil {
		t.Fatal(err)
	}
}
