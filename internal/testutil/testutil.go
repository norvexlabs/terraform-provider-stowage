// Package testutil holds the tests' shared helpers.
package testutil

import (
	"os/exec"
	"strings"
	"testing"
)

// WriteExecutable writes a program a test then runs (mode 0755), from a short-lived child
// process: a file this process held open for writing could be inherited by a parallel test's
// fork until its exec, and running it then fails with "text file busy" (ETXTBSY).
func WriteExecutable(t testing.TB, path, content string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `cat > "$1" && chmod 0755 "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing %s: %v %s", path, err, out)
	}
}
