//go:build !windows && !linux

package main

import "fmt"

func startPTY(string, int, int) (PTY, error) {
	return nil, fmt.Errorf("interactive terminal not supported on this OS")
}
