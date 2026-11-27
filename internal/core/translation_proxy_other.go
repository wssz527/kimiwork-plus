//go:build !windows

package core

import (
	"net/http"
	"net/url"
)

func translationProxy(req *http.Request) (*url.URL, error) {
	return http.ProxyFromEnvironment(req)
}
