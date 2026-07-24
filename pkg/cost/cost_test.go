/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package cost

import (
	"math"
	"testing"
)

func almostEqual(t *testing.T, got, want USD, label string) {
	t.Helper()
	if math.Abs(float64(got-want)) > 1e-9 {
		t.Errorf("%s: got %v, want %v", label, float64(got), float64(want))
	}
}

func TestUSDString(t *testing.T) {
	cases := []struct {
		name string
		in   USD
		want string
	}{
		{"negative renders as em dash", -12.5, "—"},
		{"zero renders as em dash", 0, "—"},
		{"sub-dollar keeps two decimals", 0.05, "$0.05"},
		{"half dollar keeps two decimals", 0.5, "$0.50"},
		{"just under a dollar still uses the decimal branch", 0.999, "$1.00"},
		{"exactly one dollar drops decimals", 1, "$1"},
		{"whole dollars drop decimals", 124.4, "$124"},
		{"large value drops decimals", 1121.28, "$1121"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("USD(%v).String() = %q, want %q", float64(tc.in), got, tc.want)
			}
		})
	}
}

func TestHoursPerMonthIsAWSStandardMonth(t *testing.T) {
	if HoursPerMonth != 730 {
		t.Errorf("HoursPerMonth = %d, want 730", HoursPerMonth)
	}
}

func TestSum(t *testing.T) {
	if got := Sum(nil); got != 0 {
		t.Errorf("Sum(nil) = %v, want 0", float64(got))
	}
	if got := Sum([]USD{}); got != 0 {
		t.Errorf("Sum(empty) = %v, want 0", float64(got))
	}
	almostEqual(t, Sum([]USD{1.5, 2.25, 0.25}), 4, "Sum")
	almostEqual(t, Sum([]USD{5, -2}), 3, "Sum with negative")
}

func TestProrate(t *testing.T) {
	t.Run("splits actual by allocated share", func(t *testing.T) {
		almostEqual(t, Prorate(100, 25, 100, 9), 25, "quarter share")
		almostEqual(t, Prorate(80, 40, 160, 9), 20, "eighth share")
	})

	t.Run("full share returns the whole actual", func(t *testing.T) {
		almostEqual(t, Prorate(42, 10, 10, 9), 42, "full share")
	})

	t.Run("zero allocated contributes nothing", func(t *testing.T) {
		almostEqual(t, Prorate(100, 0, 100, 9), 0, "zero allocated")
	})

	t.Run("non-positive total falls back", func(t *testing.T) {
		almostEqual(t, Prorate(100, 25, 0, 7.5), 7.5, "zero total")
		almostEqual(t, Prorate(100, 25, -1, 7.5), 7.5, "negative total")
	})
}
