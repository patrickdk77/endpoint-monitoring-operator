package driver

import (
	"errors"
	"net/url"
	"regexp"
)

var stableText = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`\b((?:dial|read|write|close) (?:tcp|udp)[46]?) [^ ]+: `), "$1: "},
	{regexp.MustCompile(`\blookup (\S+) on \S+: `), "lookup $1: "},
	{regexp.MustCompile(`stream ID \d+; `), ""},
	{regexp.MustCompile(`LastStreamID=\d+, `), ""},
	{regexp.MustCompile(`\b([A-Za-z]+ "https?://[^"?]*)\?(?:[^"\\]|\\.)*"`), `$1"`},
	{regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*://[^:@/\s"]*):[^@/\s"]*@`), `$1:***@`},
	{regexp.MustCompile(`certificate has expired or is not yet valid: current time \S+ is after (\S+)`), "certificate expired at $1"},
	{regexp.MustCompile(`certificate has expired or is not yet valid: current time \S+ is before (\S+)`), "certificate is not valid before $1"},
	{regexp.MustCompile(`\bconnection\(([^()\[]*)\[-\d+\]\)`), "connection($1)"},
}

func errorText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Op == "parse" {
		return "the endpoint is not a valid URL"
	}
	msg := err.Error()
	for _, s := range stableText {
		msg = s.pattern.ReplaceAllString(msg, s.replace)
	}
	return msg
}
