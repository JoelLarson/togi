// Package adapter defines the vendor-neutral process boundary for agent CLIs.
// Briefs enter on stdin, while raw protocol output is persisted through a Sink
// and only provider-neutral usage leaves the package.
package adapter
