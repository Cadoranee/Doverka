package clock

import (
	"sync"
	"testing"
	"time"
)

var (
	_ Clock = System{}
	_ Clock = NewFake(time.Unix(0, 0))
)

func TestSystem_Now_returnsUTCAndWithinTwoSecondsOfWallUTC(t *testing.T) {
	var s System
	got := s.Now()
	if got.Location() != time.UTC {
		t.Fatalf("Location: got %v, want UTC", got.Location())
	}
	ref := time.Now().UTC()
	diff := got.Sub(ref)
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("Now: got %v, ref %v, want within ±2s", got, ref)
	}
}

func TestNewFake_preservesInstantWithNowInUTCAndEqualToInput(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	input := time.Date(2024, 6, 15, 18, 30, 0, 0, msk)
	f := NewFake(input)
	now := f.Now()
	if now.Location() != time.UTC {
		t.Fatalf("Location: got %v, want UTC", now.Location())
	}
	if !now.Equal(input) {
		t.Errorf("Now: got %v, want Equal to input %v", now, input)
	}
}

func TestFake_Set_replacesInstantAndNormalizesToUTC(t *testing.T) {
	f := NewFake(time.Unix(1000, 0))
	pst := time.FixedZone("PST", -8*3600)
	f.Set(time.Date(2005, 3, 4, 10, 0, 0, 0, pst))
	got := f.Now()
	if got.Location() != time.UTC {
		t.Fatalf("Location: got %v, want UTC", got.Location())
	}
	want := time.Date(2005, 3, 4, 18, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Now: got %v, want %v", got, want)
	}
}

func TestFake_Advance_addsDurationToStoredInstant(t *testing.T) {
	base := time.Date(2019, 11, 7, 12, 0, 0, 0, time.UTC)
	f := NewFake(base)
	f.Advance(2*time.Hour + 30*time.Minute + 5*time.Second)
	got := f.Now()
	if got.Location() != time.UTC {
		t.Fatalf("Location: got %v, want UTC", got.Location())
	}
	want := time.Date(2019, 11, 7, 14, 30, 5, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Now: got %v, want %v", got, want)
	}
}

func TestFake_concurrentAdvanceKeepsUTCAndExactSum(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(base)
	const workers = 8
	const steps = 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < steps; j++ {
				f.Advance(time.Millisecond)
				now := f.Now()
				if now.Location() != time.UTC {
					t.Errorf("Location: got %v, want UTC", now.Location())
				}
			}
		}()
	}
	wg.Wait()
	want := base.Add(time.Duration(workers*steps) * time.Millisecond)
	got := f.Now()
	if got.Location() != time.UTC {
		t.Fatalf("Location: got %v, want UTC", got.Location())
	}
	if !got.Equal(want) {
		t.Errorf("Now: got %v, want %v", got, want)
	}
}
