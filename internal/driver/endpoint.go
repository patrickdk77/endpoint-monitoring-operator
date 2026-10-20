package driver

import (
	"net"
	"strings"
)

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
