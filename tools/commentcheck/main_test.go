package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const strayText = "package sample\n\n// narrates the body\nfunc helper() {}\n"

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func directoryWith(tb testing.TB, name, text string) string {
	tb.Helper()
	directory := tb.TempDir()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(text), 0o600); err != nil {
		tb.Fatal(err)
	}
	return directory
}

func TestCheckWithoutDirectoryIsAUsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if got := check(nil, &stdout, &stderr); got != exitUsage {
		t.Fatalf("check() = %d, want %d", got, exitUsage)
	}
	if want := "commentcheck: give at least one directory to check\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want none", stdout.String())
	}
}

func TestCheckOfAMissingDirectoryIsNoInput(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing")
	if got := check([]string{missing}, &stdout, &stderr); got != exitNoInput {
		t.Fatalf("check() = %d, want %d", got, exitNoInput)
	}
	if want := `commentcheck: cannot read "` + missing + `"`; !strings.HasPrefix(stderr.String(), want) {
		t.Fatalf("stderr = %q, want prefix %q", stderr.String(), want)
	}
}

func TestCheckOfACleanTreeIsClean(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	clean := directoryWith(t, "clean.go", "// Package sample is documented.\npackage sample\n")
	if got := check([]string{clean}, &stdout, &stderr); got != exitClean {
		t.Fatalf("check() = %d, want %d", got, exitClean)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q, want both empty", stdout.String(), stderr.String())
	}
}

func TestCheckListsEachStrayOnStdoutAndExitsOne(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	directory := directoryWith(t, "stray.go", strayText)
	if got := check([]string{directory}, &stdout, &stderr); got != exitStray {
		t.Fatalf("check() = %d, want %d", got, exitStray)
	}
	want := `"` + filepath.Join(directory, "stray.go") + `":3:1: ` + refusedReason + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestASymbolicLinkIsListedAndItsTargetIsNeverRead(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	directory := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(target, []byte("package sample\n\n// SECRETTOKEN\nfunc helper() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "evil.go")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := check([]string{directory}, &stdout, &stderr); got != exitStray {
		t.Fatalf("check() = %d, want %d", got, exitStray)
	}
	if want := `"` + link + `": ` + unreadableReason + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String()+stderr.String(), "SECRETTOKEN") {
		t.Fatalf("the target of a symbolic link was read: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestAFileThatDoesNotParseIsNoInputAndItsTextIsNotEchoed(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	directory := directoryWith(t, "bad.go", "package sample\n\nvar SECRETTOKEN = (\n")
	if got := check([]string{directory}, &stdout, &stderr); got != exitNoInput {
		t.Fatalf("check() = %d, want %d", got, exitNoInput)
	}
	if want := `commentcheck: cannot parse "` + filepath.Join(directory, "bad.go") + `":`; !strings.HasPrefix(stderr.String(), want) {
		t.Fatalf("stderr = %q, want prefix %q", stderr.String(), want)
	}
	if strings.Contains(stderr.String(), "SECRETTOKEN") || stdout.Len() != 0 {
		t.Fatalf("parser text was echoed: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestAFailedReportWriteIsIOError(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	directory := directoryWith(t, "stray.go", strayText)
	if got := check([]string{directory}, failingWriter{}, &stderr); got != exitIOError {
		t.Fatalf("check() = %d, want %d", got, exitIOError)
	}
	if !strings.HasPrefix(stderr.String(), "commentcheck: ") {
		t.Fatalf("stderr = %q, want the diagnostic", stderr.String())
	}
}

func TestAFailedDiagnosticWriteIsIOError(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	if got := check(nil, &stdout, failingWriter{}); got != exitIOError {
		t.Fatalf("check() = %d, want %d", got, exitIOError)
	}
}
