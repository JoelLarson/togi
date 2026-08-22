// Package runner is the single bounded-execution primitive beneath every
// external process togi starts: one command, a context deadline,
// whole-process-group kill on cancellation, and stdout/stderr captured into
// fixed-size buffers with an explicit truncation marker. Gates, version
// probes, and git all go through it; nothing else in togi spawns a process.
package runner
