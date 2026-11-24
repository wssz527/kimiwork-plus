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

func TestInitialWindowBackground(t *testing.T) {
	if got := initialWindowBackground(false); got != themeLightBG {
		t.Fatalf("initialWindowBackground(false) = %+v, want %+v", got, themeLightBG)
	}
	if got := initialWindowBackground(true); got != themeDarkBG {
		t.Fatalf("initialWindowBackground(true) = %+v, want %+v", got, themeDarkBG)
	}
}
