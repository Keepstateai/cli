// shared_build_test: the suite builds the client's binary ONCE and every test
// that needs the built program runs that file. Before, each of about 365
// tests ran its own `go build` (a compile cached after the first, and a link
// that is not): about 0.5 s each on a plain machine, and 10 to 20 s each under
// Cruise's sandbox recorder (strace follows every thread of the linker), which
// made the project's own suite take hours there instead of minutes.
//
// A test that CHANGES the binary on disk (the self-update test replaces it)
// still builds its own copy: it is the one name in the guard's allow list.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

var (
	sharedBuildOnce sync.Once
	sharedBuildDir  string
	sharedBuildBin  string
	sharedBuildErr  string
)

// sharedKS builds the binary under test once (with the sabotage tag when
// KS_TEST_TAGS names one) and returns its path. The file is read-only to the
// tests that use it.
func sharedKS(t *testing.T) string {
	t.Helper()
	sharedBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ks-under-test-")
		if err != nil {
			sharedBuildErr = err.Error()
			return
		}
		sharedBuildDir = dir
		bin := filepath.Join(dir, "ks-under-test")
		build := []string{"build", "-o", bin}
		if tags := os.Getenv("KS_TEST_TAGS"); tags != "" { // a sabotage run builds the binary with its build tag
			build = append(build, "-tags", tags)
		}
		if out, err := exec.Command("go", append(build, ".")...).CombinedOutput(); err != nil {
			sharedBuildErr = "build failed: " + err.Error() + "\n" + string(out)
			return
		}
		_ = os.Chmod(bin, 0o555)
		sharedBuildBin = bin
	})
	if sharedBuildErr != "" {
		t.Fatal(sharedBuildErr)
	}
	return sharedBuildBin
}

// underTracer reports whether this process is being traced (ptrace), by the
// TracerPid line of /proc/self/status.
func underTracer() bool {
	b, err := os.ReadFile("/proc/self/status")
	return err == nil && tracerPidIn(string(b))
}

func tracerPidIn(status string) bool {
	for _, line := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(line, "TracerPid:"); ok {
			return strings.TrimSpace(v) != "0"
		}
	}
	return false
}

func TestTracerPidIsReadFromTheProcessStatus(t *testing.T) {
	if !tracerPidIn("Name:\tks\nTracerPid:\t4242\nUid:\t1\n") || tracerPidIn("Name:\tks\nTracerPid:\t0\n") || tracerPidIn("Name:\tks\n") {
		t.Fatal("TracerPid is not read as a traced / not traced answer")
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedBuildDir != "" {
		_ = os.RemoveAll(sharedBuildDir)
	}
	os.Exit(code)
}

// TestTheSuiteBuildsItsBinaryOnce is the guard: no test file runs `go build`
// of its own but the one that replaces the binary it built, and the shared
// binary is the same file for every caller.
func TestTheSuiteBuildsItsBinaryOnce(t *testing.T) {
	if sharedKS(t) != sharedKS(t) {
		t.Fatal("two calls built two binaries")
	}
	re := regexp.MustCompile(`exec\.Command\("go",[^)]*"build"`)
	files, _ := filepath.Glob("*_test.go")
	var offenders []string
	for _, f := range files {
		if f == "shared_build_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) || strings.Contains(line, `"build", "-o"`) {
				offenders = append(offenders, f)
				break
			}
		}
	}
	sort.Strings(offenders)
	allowed := []string{"provenance_test.go"} // TestUpdateReplacesNothingWhenGhRejectsTheNewBinary replaces its binary
	if strings.Join(offenders, ",") != strings.Join(allowed, ",") {
		t.Fatalf("test files that run their own go build: %v (only %v may)", offenders, allowed)
	}
}
