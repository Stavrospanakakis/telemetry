// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package crashmonitor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"golang.org/x/telemetry/internal/counter"
)

func TestMain(m *testing.M) {
	entry := os.Getenv("CRASHMONITOR_TEST_ENTRYPOINT")
	switch entry {
	case "via-stderr":
		// This mode bypasses Start and debug.SetCrashOutput;
		// the crash is printed to stderr.
		debug.SetTraceback("system")
		writeSentinel(os.Stderr)

		child() // this line is "TestMain:9"
		panic("unreachable")

	case "start.panic", "start.exit":
		// These modes uses Start and debug.SetCrashOutput.
		// We stub the actual telemetry by instead writing to a file.
		incrementCounter = func(name string) {
			os.WriteFile(os.Getenv("TELEMETRY_FILE"), []byte(name), 0666)
		}
		Start()
		if entry == "start.panic" {
			go func() {
				child() // this line is "TestMain.func2:1"
			}()
			select {} // deadlocks when reached
		} else {
			os.Exit(42)
		}

	default:
		os.Exit(m.Run()) // run tests as normal
	}
}

//go:noinline
func child() {
	fmt.Println("hello")
	grandchild() // this line is "child:2"
}

//go:noinline
func grandchild() {
	panic("oops") // this line is "grandchild:1"
}

// TestViaStderr is an internal test that asserts that the telemetry
// stack generated by the panic in grandchild is correct. It uses
// stderr, and does not rely on [Start] or [debug.SetCrashOutput].
func TestViaStderr(t *testing.T) {
	_, stderr := runSelf(t, "via-stderr")
	got, err := telemetryCounterName(stderr)
	if err != nil {
		t.Fatal(err)
	}
	got = sanitize(counter.DecodeStack(got))
	want := "crash/crash\n" +
		"runtime.gopanic:--\n" +
		"golang.org/x/telemetry/crashmonitor.grandchild:1\n" +
		"golang.org/x/telemetry/crashmonitor.child:2\n" +
		"golang.org/x/telemetry/crashmonitor.TestMain:9\n" +
		"main.main:--\n" +
		"runtime.main:--\n" +
		"runtime.goexit:--"
	if got != want {
		t.Errorf("got counter name <<%s>>, want <<%s>>", got, want)
	}
}

// TestStart is an integration test of [Start]. Requires go1.23+.
func TestStart(t *testing.T) {
	if !Supported() {
		t.Skip("crashmonitor not supported")
	}

	// Assert that the crash monitor does nothing when the child
	// process merely exits.
	t.Run("exit", func(t *testing.T) {
		telemetryFile, _ := runSelf(t, "start.exit")
		data, err := os.ReadFile(telemetryFile)
		if err == nil {
			t.Fatalf("telemetry counter <<%s>> was unexpectedly incremented", data)
		}
	})

	// Assert that the crash monitor increments a telemetry
	// counter of the correct name when the child process panics.
	t.Run("panic", func(t *testing.T) {
		// Gather a stack trace from executing the panic statement above.
		telemetryFile, _ := runSelf(t, "start.panic")
		data, err := os.ReadFile(telemetryFile)
		if err != nil {
			t.Fatalf("failed to read file: %v", err)
		}
		got := sanitize(counter.DecodeStack(string(data)))
		want := "crash/crash\n" +
			"runtime.gopanic:--\n" +
			"golang.org/x/telemetry/crashmonitor.grandchild:1\n" +
			"golang.org/x/telemetry/crashmonitor.child:2\n" +
			"golang.org/x/telemetry/crashmonitor.TestMain.func2:1\n" +
			"runtime.goexit:--"
		if got != want {
			t.Errorf("got counter name <<%s>>, want <<%s>>", got, want)
		}
	})
}

// runSelf fork+exec's this test executable using an alternate entry point.
// It returns the child's stderr, and the name of the file
// to which any incremented counter name will be written.
func runSelf(t *testing.T, entrypoint string) (string, []byte) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Provide the name of the file to the child via the environment.
	// The telemetry operation may be stubbed to write to this file.
	telemetryFile := filepath.Join(t.TempDir(), "fake.telemetry")

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"CRASHMONITOR_TEST_ENTRYPOINT="+entrypoint,
		"TELEMETRY_FILE="+telemetryFile)
	cmd.Stderr = new(bytes.Buffer)
	cmd.Run() // failure is expected
	stderr := cmd.Stderr.(*bytes.Buffer).Bytes()
	if true { // debugging
		t.Logf("stderr: %s", stderr)
	}
	return telemetryFile, stderr
}

// sanitize redacts the line numbers that we don't control from a counter name.
func sanitize(name string) string {
	lines := strings.Split(name, "\n")
	for i, line := range lines {
		if symbol, _, ok := strings.Cut(line, ":"); ok &&
			!strings.HasPrefix(line, "golang.org/x/telemetry/crashmonitor") {
			lines[i] = symbol + ":--"
		}
	}
	return strings.Join(lines, "\n")
}
