package driver

import (
	"errors"
	"net"
	"strings"
)

var errNotHostPort = errors.New("endpoint must be a host or host:port, without a scheme or a user")

func checkHostEndpoint(endpoint string) error {
	if strings.Contains(endpoint, "://") || strings.Contains(endpoint, "@") {
		return errNotHostPort
	}
	return nil
}

func hostPort(endpoint, defaultPort string) (string, string) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
		port = ""
	}
	if port == "" {
		port = defaultPort
	}
	return host, port
}
