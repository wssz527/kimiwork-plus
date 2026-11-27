//go:build windows

package core

import (
	"net/http"
	"net/url"

	"golang.org/x/sys/windows/registry"
)

func translationProxy(req *http.Request) (*url.URL, error) {
	if proxy, err := http.ProxyFromEnvironment(req); proxy != nil || err != nil {
		return proxy, err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return nil, nil
	}
	defer key.Close()
	enabled, kind, err := key.GetIntegerValue("ProxyEnable")
	if err != nil || kind != registry.DWORD || enabled != 1 {
		return nil, nil
	}
	server, _, err := key.GetStringValue("ProxyServer")
	if err != nil {
		return nil, nil
	}
	override, _, _ := key.GetStringValue("ProxyOverride")
	return parseWindowsProxyConfig(server, override, req.URL)
}
