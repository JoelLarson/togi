package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/joellarson/togi/internal/waiver"
)

const commandFingerprint = "0f2ac1e1b8f7c8b56b6da5e0f9dc0f6e6c1a2b3c4d5e6f708192a3b4c5d6e7f8"

func TestWaiveCommandPassesFingerprintAndReason(t *testing.T) {
	service := &fakeService{}
	cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
	cmd.SetArgs([]string{"waive", commandFingerprint, "--reason", "the deleted test covered a removed feature"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if service.waiveRoot != "." || service.waiveFingerprint != commandFingerprint {
		t.Fatalf("waive root = %q, fingerprint = %q", service.waiveRoot, service.waiveFingerprint)
	}
	if service.waiveReason != "the deleted test covered a removed feature" {
		t.Fatalf("waive reason = %q", service.waiveReason)
	}
}

func TestWaiveCommandRequiresExactlyOneFingerprint(t *testing.T) {
	for _, args := range [][]string{
		{"waive"},
		{"waive", "--reason", "approved"},
		{"waive", commandFingerprint, commandFingerprint, "--reason", "approved"},
	} {
		service := &fakeService{}
		cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("%v was accepted", args)
		}
		if service.waiveCalls != 0 {
			t.Fatalf("%v reached the service", args)
		}
	}
}

// An absent reason reaches the service, so the in-process and compiled
// boundaries refuse an unexplained approval through the same rule.
func TestWaiveCommandLeavesTheReasonRuleToTheService(t *testing.T) {
	for _, args := range [][]string{
		{"waive", commandFingerprint},
		{"waive", commandFingerprint, "--reason", ""},
	} {
		service := &fakeService{waiveErr: waiver.ErrReasonRequired}
		cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
		cmd.SetArgs(args)
		if err := cmd.Execute(); !errors.Is(err, waiver.ErrReasonRequired) {
			t.Fatalf("Execute(%v) = %v, want the service rule", args, err)
		}
		if service.waiveCalls != 1 || service.waiveReason != "" {
			t.Fatalf("%v reached the service %d times with reason %q", args, service.waiveCalls, service.waiveReason)
		}
	}
}

func TestWaiveCommandPropagatesServiceErrors(t *testing.T) {
	service := &fakeService{waiveErr: waiver.ErrReasonRequired}
	cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
	cmd.SetArgs([]string{"waive", commandFingerprint, "--reason", "approved"})
	err := cmd.Execute()
	if !errors.Is(err, waiver.ErrReasonRequired) {
		t.Fatalf("Execute() = %v, want the service error", err)
	}
	var exitCoder interface{ ExitCode() int }
	if errors.As(err, &exitCoder) {
		t.Fatalf("Execute() = %v, want an error without a run exit code", err)
	}
}

func TestWaiveCommandIsListedInHelp(t *testing.T) {
	var stdout bytes.Buffer
	cmd := newRootCommandWithService(streams{out: &stdout, err: io.Discard}, &fakeService{})
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "waive") {
		t.Fatalf("help omits waive:\n%s", stdout.String())
	}
}

func TestWaiveCommandUsesTheCommandContext(t *testing.T) {
	service := &fakeService{}
	cmd := newRootCommandWithService(streams{out: io.Discard, err: io.Discard}, service)
	ctx := context.WithValue(context.Background(), waiveContextKey{}, "carried")
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"waive", commandFingerprint, "--reason", "approved"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if service.ctx == nil || service.ctx.Value(waiveContextKey{}) != "carried" {
		t.Fatal("waive did not receive the command context")
	}
}

type waiveContextKey struct{}
