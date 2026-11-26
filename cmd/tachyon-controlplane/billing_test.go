package main

import (
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/billing"
)

func TestParseRunnerPrices(t *testing.T) {
	got, err := parseRunnerPrices("2=0.004, 4=0.008,16=0.032")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]billing.Money{2: 4000, 4: 8000, 16: 32000}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%d vcpu = %v, want %v", k, got[k], v)
		}
	}
	for _, bad := range []string{"", "4", "x=1", "0=1", "4=-1", "4=abc", "4=1,4=2"} {
		if _, err := parseRunnerPrices(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestPreviousMonth(t *testing.T) {
	got := previousMonth(time.Date(2026, 1, 15, 3, 0, 0, 0, time.UTC))
	if want := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
