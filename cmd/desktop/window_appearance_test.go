package main

import "testing"

func TestNormalizeThemeMode(t *testing.T) {
	cases := map[string]string{
		"light":   themeModeLight,
		"dark":    themeModeDark,
		"auto":    themeModeAuto,
		"":        themeModeAuto,
		"unknown": themeModeAuto,
	}
	for input, want := range cases {
		if got := normalizeThemeMode(input); got != want {
			t.Fatalf("normalizeThemeMode(%q) = %q, want %q", input, got, want)
		}
	}
}
