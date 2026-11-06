//go:build !windows

package main

// runTray is a no-op off Windows; the dashboard is the management UI there.
func runTray(srv *Server, quit func()) {}
