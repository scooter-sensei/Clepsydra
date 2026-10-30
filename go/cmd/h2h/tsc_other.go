//go:build !amd64

package main

import "time"

var epoch = time.Now()

// rdtscp stands in for the time-stamp counter off amd64: nanoseconds.
func rdtscp() uint64 { return uint64(time.Since(epoch)) }
