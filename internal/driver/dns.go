package driver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	v1 "github.com/LiciousTech/endpoint-monitoring-operator/api/v1alpha1"
)

type DNSDriver struct {
	endpoint string
	check    *v1.DnsCheck
}

func NewDNSDriver(endpoint string, check *v1.DnsCheck) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}

	return &DNSDriver{endpoint: endpoint, check: check}, nil
}

func (d *DNSDriver) Check() (*CheckResult, error) {
	start := time.Now()

	var err error
	var resolver *net.Resolver
	if d.check == nil || d.check.Server == "" { // Use default dns resolver
		resolver = &net.Resolver{
			PreferGo: true,
			Dial:     nil,
		}
	} else {
		hostPart, port, err := net.SplitHostPort(d.check.Server)
		if err == nil {
			if port == "" {
				port = "53"
				if d.check.Tls {
					port = "853"
				}
			}
			host, err := net.LookupHost(hostPart) // We need to specify the dns server by ip address, so make sure
			if err == nil {
				server := net.JoinHostPort(host[0], port)
				if d.check.Tls { // use DNSoverTLS server
					tlsConfig := &tls.Config{ServerName: hostPart}
					resolver = &net.Resolver{
						PreferGo: true,
						Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
							dialer := &net.Dialer{Timeout: 10 * time.Second}
							tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
							return tlsDialer.DialContext(ctx, network, server)
						},
					}
				} else { // Use normal specific dns server
					resolver = &net.Resolver{
						PreferGo: true,
						Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
							dialer := &net.Dialer{Timeout: 10 * time.Second}
							return dialer.DialContext(ctx, network, server)
						},
					}
				}
			}
		}
	}

	if err == nil && resolver != nil {
		switch strings.ToUpper(d.check.Type) {
		case "TXT":
			values, err := resolver.LookupTXT(context.Background(), d.endpoint)
			if err == nil && d.check.Verify != "" {
				match := false
				for _, val := range values {
					if val == d.check.Verify {
						match = true
					}
				}
				if !match {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		case "CNAME":
			value, err := resolver.LookupCNAME(context.Background(), d.endpoint)
			if err == nil && d.check.Verify != "" {
				if value != d.check.Verify {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		case "PTR":
			values, err := resolver.LookupAddr(context.Background(), d.endpoint)
			if err == nil && d.check.Verify != "" {
				match := false
				for _, val := range values {
					if val == d.check.Verify {
						match = true
					}
				}
				if !match {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		case "AAAA":
			values, err := resolver.LookupIP(context.Background(), "ip6", d.endpoint)
			if err == nil && d.check.Verify != "" {
				ip := net.ParseIP(d.check.Verify)
				match := false
				for _, val := range values {
					if val.Equal(ip) {
						match = true
					}
				}
				if !match {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		case "A":
			values, err := resolver.LookupIP(context.Background(), "ip4", d.endpoint)
			if err == nil && d.check.Verify != "" {
				ip := net.ParseIP(d.check.Verify)
				match := false
				for _, val := range values {
					if val.Equal(ip) {
						match = true
					}
				}
				if !match {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		default:
			values, err := resolver.LookupHost(context.Background(), d.endpoint)
			if err == nil && d.check.Verify != "" {
				match := false
				for _, val := range values {
					if val == d.check.Verify {
						match = true
					}
				}
				if !match {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		}
	}

	duration := time.Since(start)

	result := &CheckResult{
		ResponseTime: duration,
	}

	if err != nil {
		result.Success = false
		result.Error = err
		result.Message = fmt.Sprintf("DNS check failed: %v", err)
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("DNS check successful (response time: %v)", duration)

	return result, nil
}

func (d *DNSDriver) GetEndpoint() string {
	return d.endpoint
}

func (d *DNSDriver) GetType() string {
	return "dns"
}
