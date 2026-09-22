//go:build !linux && !darwin

package main

// Other platforms fall back to plain line-by-line input.

func makeRaw(fd int) (restore func(), ok bool) { return nil, false }

func isTerminal(fd int) bool { return false }

func termWidth(fd int) int { return 80 }
