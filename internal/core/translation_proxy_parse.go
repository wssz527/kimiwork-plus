package core

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

func parseWindowsProxyConfig(proxyServer, proxyOverride string, reqURL *url.URL) (*url.URL, error) {
	scheme := strings.ToLower(reqURL.Scheme)
	host, port := strings.ToLower(reqURL.Hostname()), reqURL.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	for _, entry := range strings.Split(proxyOverride, ";") {
		pattern := strings.ToLower(strings.TrimSpace(entry))
		if pattern == "" {
			continue
		}
		if pattern == "<local>" {
			if !strings.Contains(host, ".") || net.ParseIP(host).IsLoopback() {
				return nil, nil
			}
			continue
		}
		value := host
		if _, _, err := net.SplitHostPort(pattern); err == nil && port != "" {
			value = net.JoinHostPort(host, port)
		}
		if matchProxyOverride(pattern, value) {
			return nil, nil
		}
	}

	address := strings.TrimSpace(proxyServer)
	proxyScheme := "http"
	if strings.Contains(address, "=") {
		proxies := make(map[string]string)
		for _, entry := range strings.Split(address, ";") {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				proxies[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
			}
		}
		address = proxies[scheme]
		if address == "" {
			address = proxies["http"]
		}
		if address == "" {
			address, proxyScheme = proxies["socks"], "socks5"
		}
	}
	if address == "" {
		return nil, nil
	}
	if !strings.Contains(address, "://") {
		address = proxyScheme + "://" + address
	}
	proxy, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	if proxy.Hostname() == "" {
		return nil, errors.New("invalid_windows_proxy")
	}
	if proxy.Scheme == "socks" {
		proxy.Scheme = "socks5"
	}
	return proxy, nil
}

func matchProxyOverride(pattern, value string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	value, matched := strings.CutPrefix(value, parts[0])
	if !matched {
		return false
	}
	value, matched = strings.CutSuffix(value, parts[len(parts)-1])
	if !matched {
		return false
	}
	for _, part := range parts[1 : len(parts)-1] {
		if _, remainder, found := strings.Cut(value, part); found {
			value = remainder
		} else {
			return false
		}
	}
	return true
}
