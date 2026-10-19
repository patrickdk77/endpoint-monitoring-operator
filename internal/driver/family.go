package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const (
	FamilyIPv4 = "ipv4"
	FamilyIPv6 = "ipv6"
	FamilyAll  = "all"
)

// familyNetwork narrows a "tcp" or "udp" network to the address family
// requested by the monitor; an empty family leaves it unchanged.
func familyNetwork(network, family string) string {
	base := strings.TrimRight(network, "46")
	switch family {
	case FamilyIPv4:
		return base + "4"
	case FamilyIPv6:
		return base + "6"
	}
	return network
}

func httpClient(family string, timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if family == "" {
		return client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = familyDial(family)
	transport.DisableKeepAlives = true
	client.Transport = transport
	return client
}

func familyDial(family string) func(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, familyNetwork(network, family), address)
	}
}

func pickFamily(addrs []string, family string) (string, error) {
	for _, a := range addrs {
		if family == "" {
			return a, nil
		}
		ip, err := netip.ParseAddr(a)
		if err != nil {
			continue
		}
		if family == FamilyIPv4 && ip.Unmap().Is4() {
			return a, nil
		}
		if family == FamilyIPv6 && ip.Is6() && !ip.Is4In6() {
			return a, nil
		}
	}
	return "", fmt.Errorf("no %s address in %v", family, addrs)
}

type bothFamiliesDriver struct {
	v4 Driver
	v6 Driver
}

// NewBothFamiliesDriver checks over IPv4 and then IPv6; the result is a
// success only when both checks succeed.
func NewBothFamiliesDriver(v4, v6 Driver) Driver {
	return &bothFamiliesDriver{v4: v4, v6: v6}
}

func (b *bothFamiliesDriver) Check() (*CheckResult, error) {
	r4, err := b.v4.Check()
	if err != nil {
		return nil, fmt.Errorf("ipv4: %w", err)
	}
	r6, err := b.v6.Check()
	if err != nil {
		return nil, fmt.Errorf("ipv6: %w", err)
	}

	var errs []error
	for _, r := range []struct {
		family string
		result *CheckResult
	}{{FamilyIPv4, r4}, {FamilyIPv6, r6}} {
		if r.result.Success {
			continue
		}
		err := r.result.Error
		if err == nil {
			err = errors.New(r.result.Message)
		}
		errs = append(errs, fmt.Errorf("%s: %w", r.family, err))
	}

	return &CheckResult{
		Success:      r4.Success && r6.Success,
		ResponseTime: max(r4.ResponseTime, r6.ResponseTime),
		Error:        errors.Join(errs...),
		Message:      fmt.Sprintf("ipv4: %s; ipv6: %s", r4.Message, r6.Message),
	}, nil
}

func (b *bothFamiliesDriver) GetEndpoint() string {
	return b.v4.GetEndpoint()
}

func (b *bothFamiliesDriver) GetType() string {
	return b.v4.GetType()
}
