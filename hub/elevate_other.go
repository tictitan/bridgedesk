//go:build !windows

package main

// Elevation is a Windows concept; on POSIX run with sudo if you need root.
func isElevated() bool       { return false }
func relaunchElevated() bool { return false }
