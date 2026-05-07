package version

// Version is set at build time via -ldflags.
var Version = "dev"

// UserAgent is the HTTP User-Agent header value used by all outbound HTTP requests.
var UserAgent = "endpoint-monitoring/" + Version
