//go:build !race

// Package racecheck tells tests whether the race detector is on. The detector
// adds allocations of its own, so zero-allocation assertions must skip.
package racecheck

// Enabled is true when built with -race.
const Enabled = false
