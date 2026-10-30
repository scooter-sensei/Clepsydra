//go:build amd64

package main

// rdtscp reads the time-stamp counter once the instructions before it have
// executed.
func rdtscp() uint64
