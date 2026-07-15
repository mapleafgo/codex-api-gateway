// Package main starts the CodexApiGateway HTTP server.
package main

import "net/http"

// httpListenAndServe is a thin wrapper around http.ListenAndServe so that
// main.go does not need to import net/http solely for this one call.
func httpListenAndServe(addr string, h http.Handler) error {
	return http.ListenAndServe(addr, h)
}
