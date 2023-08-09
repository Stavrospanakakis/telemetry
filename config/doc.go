// The config package holds the config.json file defining the Go telemetry
// upload configuration.
//
// An upload configuration specifies the set of values that are permitted in
// telemetry uploads: GOOS, GOARCH, Go version, and per-program counters.
//
// This package contains no actual Go code, and exists only so the config.json
// file can be served by module proxies.
package config
