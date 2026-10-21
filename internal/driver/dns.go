package driver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	v1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
)

type DNSDriver struct {
	endpoint string
	family   string
	check    *v1.DnsCheck
}

func NewDNSDriver(endpoint, family string, check *v1.DnsCheck) (Driver, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("endpoint cannot be empty")
	}
	if err := checkHostEndpoint(endpoint); err != nil {
		return nil, err
	}
	if check == nil {
		check = &v1.DnsCheck{}
	}

	return &DNSDriver{endpoint: endpoint, family: family, check: check}, nil
}

func (d *DNSDriver) Check() (*CheckResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	var err error = nil
	var resolver *net.Resolver
	if d.check.Server == "" { // Use default dns resolver
		resolver = &net.Resolver{
			PreferGo: true,
			Dial:     nil,
		}
		if d.family != "" {
			resolver.Dial = familyDial(d.family)
		}
	} else {
		defaultPort := "53"
		if d.check.Tls {
			defaultPort = "853"
		}
		hostPart, port := hostPort(d.check.Server, defaultPort)
		var host []string
		host, err = net.DefaultResolver.LookupHost(ctx, hostPart) // We need to specify the dns server by ip address, so make sure
		var addr string
		if err == nil {
			addr, err = pickFamily(host, d.family)
		}
		if err == nil {
			server := net.JoinHostPort(addr, port)
			if d.check.Tls { // use DNSoverTLS server
				tlsConfig := &tls.Config{ServerName: hostPart}
				resolver = &net.Resolver{
					PreferGo: true,
					Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
						dialer := &net.Dialer{Timeout: 10 * time.Second}
						tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
						return tlsDialer.DialContext(ctx, familyNetwork("tcp", d.family), server)
					},
				}
			} else { // Use normal specific dns server
				resolver = &net.Resolver{
					PreferGo: true,
					Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
						dialer := &net.Dialer{Timeout: 10 * time.Second}
						return dialer.DialContext(ctx, familyNetwork(network, d.family), server)
					},
				}
			}
		}
	}

	if err == nil && resolver != nil {
		switch strings.ToUpper(d.check.Type) {
		case "TXT":
			var values []string
			values, err = resolver.LookupTXT(ctx, d.endpoint)
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
			var value string
			value, err = resolver.LookupCNAME(ctx, d.endpoint)
			if err == nil && d.check.Verify != "" {
				if !sameName(value, d.check.Verify) {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		case "PTR":
			var values []string
			values, err = resolver.LookupAddr(ctx, d.endpoint)
			if err == nil && d.check.Verify != "" {
				match := false
				for _, val := range values {
					if sameName(val, d.check.Verify) {
						match = true
					}
				}
				if !match {
					err = fmt.Errorf("returned dns result did not match Verify value")
				}
			}
		case "AAAA":
			var values []net.IP
			values, err = resolver.LookupIP(ctx, "ip6", d.endpoint)
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
			var values []net.IP
			values, err = resolver.LookupIP(ctx, "ip4", d.endpoint)
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
			var values []string
			values, err = resolver.LookupHost(ctx, d.endpoint)
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
		result.Message = fmt.Sprintf("DNS check failed: %s", errorText(err))
		return result, nil
	}

	result.Success = true
	result.Message = fmt.Sprintf("DNS check successful (response time: %v)", duration)

	return result, nil
}

func sameName(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

func (d *DNSDriver) GetEndpoint() string {
	return d.endpoint
}

func (d *DNSDriver) GetType() string {
	return "dns"
}
