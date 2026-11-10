package kimi

import "net/url"

// parseURL is the single place a native URL is parsed, so the integration and
// the bridge cannot disagree about what a valid endpoint is.
func parseURL(value string) (*url.URL, error) { return url.Parse(value) }
