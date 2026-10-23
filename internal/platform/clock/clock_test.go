package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/clock"
)

func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, 11, 3, 14, 21, 7, 0, time.UTC)
	fake := clock.NewFake(start)

	if got := fake.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
	fake.Advance(30 * time.Minute)
	if got, want := fake.Now(), start.Add(30*time.Minute); !got.Equal(want) {
		t.Errorf("Now() after Advance = %v, want %v", got, want)
	}
}

func TestSystemNowIsUTCMicroseconds(t *testing.T) {
	now := clock.System{}.Now()

	if now.Location() != time.UTC {
		t.Errorf("Location() = %v, want UTC", now.Location())
	}
	if now.Nanosecond()%1000 != 0 {
		t.Errorf("Nanosecond() = %d, want a multiple of 1000", now.Nanosecond())
	}
}

func TestNewFakeNormalizesStart(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 3, 11, 21, 7, 123456789, loc)
	fake := clock.NewFake(start)
	now := fake.Now()
	if now.Location() != time.UTC {
		t.Errorf("Location() = %v, want UTC", now.Location())
	}
	if got := now.Nanosecond(); got != 123456000 {
		t.Errorf("Nanosecond() = %d, want 123456000", got)
	}
}

func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	start := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	fake := clock.NewFake(start)

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			fake.Advance(time.Second)
			_ = fake.Now()
		})
	}
	wg.Wait()
	if got, want := fake.Now(), start.Add(100*time.Second); !got.Equal(want) {
		t.Errorf("Now() after Advance = %v, want %v", got, want)
	}
}
