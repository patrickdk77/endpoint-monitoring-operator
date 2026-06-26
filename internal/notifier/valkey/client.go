package valkey

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	conn net.Conn
	r    *bufio.Reader
}

func Dial(endpoint string, tlsEnabled bool, username, password string, db int) (*Client, error) {
	hostPart, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		hostPart = endpoint
		port = "6379"
	}
	addr := net.JoinHostPort(hostPart, port)

	var conn net.Conn
	if tlsEnabled {
		tlsConfig := &tls.Config{ServerName: hostPart}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("dial valkey: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}

	c := &Client{conn: conn, r: bufio.NewReader(conn)}

	if password != "" {
		var authCmd []string
		if username != "" {
			authCmd = []string{"AUTH", username, password}
		} else {
			authCmd = []string{"AUTH", password}
		}
		if err := c.do(authCmd); err != nil {
			conn.Close()
			return nil, fmt.Errorf("auth: %w", err)
		}
	}

	if db > 0 {
		if err := c.do([]string{"SELECT", strconv.Itoa(db)}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("select db: %w", err)
		}
	}

	return c, nil
}

func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	_, _ = c.conn.Write([]byte("QUIT\r\n"))
	return c.conn.Close()
}

func (c *Client) Pipeline(cmds [][]string) error {
	for _, cmd := range cmds {
		if _, err := c.conn.Write(encodeCommand(cmd)); err != nil {
			return err
		}
	}
	for range cmds {
		if _, err := readReply(c.r); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) do(cmd []string) error {
	if _, err := c.conn.Write(encodeCommand(cmd)); err != nil {
		return err
	}
	_, err := readReply(c.r)
	return err
}

func encodeCommand(cmd []string) []byte {
	var b strings.Builder
	b.WriteString("*")
	b.WriteString(strconv.Itoa(len(cmd)))
	b.WriteString("\r\n")
	for _, arg := range cmd {
		b.WriteString("$")
		b.WriteString(strconv.Itoa(len(arg)))
		b.WriteString("\r\n")
		b.WriteString(arg)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 1 {
		return nil, fmt.Errorf("empty reply")
	}
	switch line[0] {
	case '+':
		return strings.TrimSuffix(line[1:], "\r\n"), nil
	case '-':
		return nil, fmt.Errorf("valkey error: %s", strings.TrimSpace(line[1:]))
	case ':':
		n, err := strconv.ParseInt(strings.TrimSpace(line[1:]), 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case '$':
		size, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		if size < 0 {
			return nil, nil
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:size]), nil
	case '*':
		count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		items := make([]any, count)
		for i := 0; i < count; i++ {
			item, err := readReply(r)
			if err != nil {
				return nil, err
			}
			items[i] = item
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unknown reply type: %q", line)
	}
}
