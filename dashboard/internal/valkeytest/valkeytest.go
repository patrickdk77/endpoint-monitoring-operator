// Package valkeytest runs an in-memory server that speaks enough RESP3 for
// valkey-go clients and the commands the dashboard and the operator's valkey
// notifier use, so tests can exercise the store without a real Valkey.
package valkeytest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type failure struct {
	cmd, keyPart, msg string
}

type Server struct {
	ln net.Listener

	mu        sync.Mutex
	hashes    map[string]map[string]string
	sets      map[string]map[string]struct{}
	keyTTL    map[string]int64
	fieldTTL  map[string]map[string]int64
	failures  []failure
	commands  [][]string
	connWG    sync.WaitGroup
	closeOnce sync.Once
}

// Start listens on a loopback port and closes the server when the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		ln:       ln,
		hashes:   map[string]map[string]string{},
		sets:     map[string]map[string]struct{}{},
		keyTTL:   map[string]int64{},
		fieldTTL: map[string]map[string]int64{},
	}
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.ln.Close()
	})
}

// Fail makes every cmd (upper case) whose first key contains keyPart answer
// with an error until Heal is called.
func (s *Server) Fail(cmd, keyPart, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failure{cmd: cmd, keyPart: keyPart, msg: msg})
}

func (s *Server) Heal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = nil
}

func (s *Server) HSet(key, field, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hset(key, field, value)
}

func (s *Server) SAdd(key string, members ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range members {
		s.sadd(key, m)
	}
}

func (s *Server) HGetAll(key string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for f, v := range s.hashes[key] {
		out[f] = v
	}
	return out
}

// KeyTTL returns the TTL in seconds last set on key with EXPIRE.
func (s *Server) KeyTTL(key string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl, ok := s.keyTTL[key]
	return ttl, ok
}

// FieldTTL returns the TTL in seconds last set on a hash field with HEXPIRE.
func (s *Server) FieldTTL(key, field string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ttl, ok := s.fieldTTL[key][field]
	return ttl, ok
}

// Commands returns every command received after the connection handshake.
func (s *Server) Commands() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.commands))
	copy(out, s.commands)
	return out
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.connWG.Add(1)
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	defer s.connWG.Done()
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		s.reply(w, args)
		if r.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("unexpected %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if len(line) == 0 || line[0] != '$' {
			return nil, fmt.Errorf("unexpected %q", line)
		}
		size, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (s *Server) reply(w *bufio.Writer, args []string) {
	if len(args) == 0 {
		writeError(w, "ERR empty command")
		return
	}
	cmd := strings.ToUpper(args[0])
	switch cmd {
	case "HELLO":
		fmt.Fprintf(w, "%%4\r\n")
		writeBulk(w, "server")
		writeBulk(w, "valkey")
		writeBulk(w, "version")
		writeBulk(w, "9.0.0")
		writeBulk(w, "proto")
		fmt.Fprintf(w, ":3\r\n")
		writeBulk(w, "mode")
		writeBulk(w, "standalone")
		return
	case "CLIENT", "SELECT", "AUTH":
		fmt.Fprintf(w, "+OK\r\n")
		return
	case "PING":
		fmt.Fprintf(w, "+PONG\r\n")
		return
	case "CLUSTER":
		writeError(w, "ERR This instance has cluster support disabled")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, args)
	if len(args) > 1 {
		for _, f := range s.failures {
			if f.cmd == cmd && strings.Contains(args[1], f.keyPart) {
				writeError(w, f.msg)
				return
			}
		}
	}

	switch cmd {
	case "HSET":
		if len(args) < 4 || len(args)%2 != 0 {
			writeError(w, "ERR wrong number of arguments for 'hset' command")
			return
		}
		added := 0
		for i := 2; i < len(args); i += 2 {
			if _, ok := s.hashes[args[1]][args[i]]; !ok {
				added++
			}
			s.hset(args[1], args[i], args[i+1])
		}
		fmt.Fprintf(w, ":%d\r\n", added)
	case "HGETALL":
		h := s.hashes[args[1]]
		fields := make([]string, 0, len(h))
		for f := range h {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		fmt.Fprintf(w, "%%%d\r\n", len(fields))
		for _, f := range fields {
			writeBulk(w, f)
			writeBulk(w, h[f])
		}
	case "HDEL":
		removed := 0
		for _, f := range args[2:] {
			if _, ok := s.hashes[args[1]][f]; ok {
				s.hdel(args[1], f)
				removed++
			}
		}
		fmt.Fprintf(w, ":%d\r\n", removed)
	case "HEXPIRE":
		// HEXPIRE key seconds FIELDS n field...
		if len(args) < 6 || strings.ToUpper(args[3]) != "FIELDS" {
			writeError(w, "ERR syntax error")
			return
		}
		ttl, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			writeError(w, "ERR value is not an integer or out of range")
			return
		}
		fields := args[5:]
		if n, err := strconv.Atoi(args[4]); err != nil || n != len(fields) {
			writeError(w, "ERR numfields does not match the number of fields")
			return
		}
		fmt.Fprintf(w, "*%d\r\n", len(fields))
		for _, f := range fields {
			if _, ok := s.hashes[args[1]][f]; !ok {
				fmt.Fprintf(w, ":-2\r\n")
				continue
			}
			if ttl <= 0 {
				s.hdel(args[1], f)
				fmt.Fprintf(w, ":2\r\n")
				continue
			}
			if s.fieldTTL[args[1]] == nil {
				s.fieldTTL[args[1]] = map[string]int64{}
			}
			s.fieldTTL[args[1]][f] = ttl
			fmt.Fprintf(w, ":1\r\n")
		}
	case "EXPIRE":
		ttl, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			writeError(w, "ERR value is not an integer or out of range")
			return
		}
		if !s.exists(args[1]) {
			fmt.Fprintf(w, ":0\r\n")
			return
		}
		if ttl <= 0 {
			s.del(args[1])
		} else {
			s.keyTTL[args[1]] = ttl
		}
		fmt.Fprintf(w, ":1\r\n")
	case "PERSIST":
		if _, ok := s.keyTTL[args[1]]; !ok {
			fmt.Fprintf(w, ":0\r\n")
			return
		}
		delete(s.keyTTL, args[1])
		fmt.Fprintf(w, ":1\r\n")
	case "SADD":
		added := 0
		for _, m := range args[2:] {
			if _, ok := s.sets[args[1]][m]; !ok {
				added++
			}
			s.sadd(args[1], m)
		}
		fmt.Fprintf(w, ":%d\r\n", added)
	case "SMEMBERS":
		members := make([]string, 0, len(s.sets[args[1]]))
		for m := range s.sets[args[1]] {
			members = append(members, m)
		}
		sort.Strings(members)
		fmt.Fprintf(w, "~%d\r\n", len(members))
		for _, m := range members {
			writeBulk(w, m)
		}
	case "DEL":
		removed := 0
		for _, k := range args[1:] {
			if s.exists(k) {
				removed++
			}
			s.del(k)
		}
		fmt.Fprintf(w, ":%d\r\n", removed)
	default:
		writeError(w, "ERR unknown command '"+args[0]+"'")
	}
}

func (s *Server) hset(key, field, value string) {
	if s.hashes[key] == nil {
		s.hashes[key] = map[string]string{}
	}
	s.hashes[key][field] = value
}

// hdel removes a field, and the key with it when the hash becomes empty, as
// Valkey does.
func (s *Server) hdel(key, field string) {
	delete(s.hashes[key], field)
	delete(s.fieldTTL[key], field)
	if len(s.hashes[key]) == 0 {
		s.del(key)
	}
}

func (s *Server) del(key string) {
	delete(s.hashes, key)
	delete(s.sets, key)
	delete(s.keyTTL, key)
	delete(s.fieldTTL, key)
}

func (s *Server) sadd(key, member string) {
	if s.sets[key] == nil {
		s.sets[key] = map[string]struct{}{}
	}
	s.sets[key][member] = struct{}{}
}

func (s *Server) exists(key string) bool {
	_, h := s.hashes[key]
	_, st := s.sets[key]
	return h || st
}

func writeBulk(w *bufio.Writer, v string) {
	fmt.Fprintf(w, "$%d\r\n%s\r\n", len(v), v)
}

func writeError(w *bufio.Writer, msg string) {
	fmt.Fprintf(w, "-%s\r\n", msg)
}
