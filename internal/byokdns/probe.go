package byokdns

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

type ProbeResult struct {
	Passed  bool   `json:"passed"`
	Address string `json:"address,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Status struct {
	Revealed bool        `json:"revealed"`
	Flag     string      `json:"flag"`
	Intranet ProbeResult `json:"intranet"`
}

type LookupFunc func(context.Context, string) (string, error)

type probeFlight struct {
	done   chan struct{}
	epoch  uint64
	result ProbeResult
}

type Probe struct {
	Lookup  LookupFunc
	mu      sync.Mutex
	cached  ProbeResult
	at      time.Time
	pending *probeFlight
	epoch   uint64
}

func (p *Probe) Invalidate() {
	p.mu.Lock()
	p.epoch++
	p.at = time.Time{}
	p.cached = ProbeResult{}
	p.mu.Unlock()
}

func (p *Probe) DNS(ctx context.Context) ProbeResult {
	for {
		p.mu.Lock()
		if !p.at.IsZero() && time.Since(p.at) < time.Minute {
			result := p.cached
			p.mu.Unlock()
			return result
		}
		if p.pending != nil {
			flight := p.pending
			p.mu.Unlock()
			select {
			case <-flight.done:
				p.mu.Lock()
				stale := p.epoch != flight.epoch
				p.mu.Unlock()
				if stale {
					continue
				}
				return flight.result
			case <-ctx.Done():
				return ProbeResult{Error: "dns_unavailable"}
			}
		}
		if ctx.Err() != nil {
			p.mu.Unlock()
			return ProbeResult{Error: "dns_unavailable"}
		}
		flight := &probeFlight{
			done:  make(chan struct{}),
			epoch: p.epoch,
		}
		p.pending = flight
		lookup := p.Lookup
		if lookup == nil {
			lookup = defaultLookup
		}
		p.mu.Unlock()

		go func() {
			lookupContext, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			address, err := lookup(lookupContext, Domain)
			cancel()

			result := ProbeResult{Address: address}
			if err != nil {
				result = ProbeResult{Error: "dns_unavailable"}
			} else {
				result.Passed = PrivateAddress(address)
			}

			p.mu.Lock()
			flight.result = result
			if flight.epoch == p.epoch && !errors.Is(err, context.Canceled) {
				p.cached = result
				p.at = time.Now()
			}
			if p.pending == flight {
				p.pending = nil
			}
			close(flight.done)
			p.mu.Unlock()
		}()

		select {
		case <-flight.done:
			return flight.result
		case <-ctx.Done():
			return ProbeResult{Error: "dns_unavailable"}
		}
	}
}

func defaultLookup(ctx context.Context, domain string) (string, error) {
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip4", domain)
	if err != nil {
		return "", err
	}
	if len(addresses) == 0 {
		return "", errors.New("dns_unavailable")
	}
	return addresses[0].String(), nil
}

func (p *Probe) Check(ctx context.Context, flagFile string) Status {
	_, flag, err := readFlag(flagFile)
	status := Status{Flag: flag, Intranet: p.DNS(ctx)}
	if err != nil {
		status.Flag = "unavailable"
		return status
	}
	status.Revealed = flag == "revealed"
	return status
}

func readFlag(file string) (map[string]any, string, error) {
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, "missing", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, "unavailable", errors.New("invalid_setup_flag")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "unavailable", errors.New("invalid_setup_flag")
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || value == nil {
		return nil, "unavailable", errors.New("invalid_setup_flag")
	}
	if value["revealed"] == true {
		return value, "revealed", nil
	}
	return value, "hidden", nil
}
