package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/norvexlabs/terraform-provider-stowage/internal/testutil"
)

// destroyStep is the TrueNAS guide's destroy-time step: the command of its `when = destroy`
// provisioner, the heredoc's indentation taken off.
func destroyStep(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/guides/truenas-vm/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)when\s+=\s+destroy\s+command\s+=\s+<<-EOT\n(.*?)\n\s*EOT`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no destroy-time heredoc in the TrueNAS guide")
	}
	lines := strings.Split(m[1], "\n")
	indent := len(lines[0]) - len(strings.TrimLeft(lines[0], " "))
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, strings.Repeat(" ", indent))
	}
	return strings.Join(lines, "\n")
}

// The TrueNAS guide's destroy step refuses, naming how to fetch it again, an installer that is
// not on this machine; one that is, it runs.
func TestTrueNASDestroyStepNeedsItsInstaller(t *testing.T) {
	t.Parallel()
	step := destroyStep(t)
	dir := t.TempDir()
	run := func(ctl string) (string, error) {
		cmd := exec.Command("sh", "-c", step)
		cmd.Env = append(os.Environ(), "CTL="+ctl, "VM_ID=7")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run(filepath.Join(dir, "gone-stow"))
	if err == nil || !strings.Contains(out, "is not on this machine") || !strings.Contains(out, "tofu apply -refresh-only") {
		t.Fatalf("a missing installer: %v %q", err, out)
	}
	ctl := filepath.Join(dir, "notes-stow")
	testutil.WriteExecutable(t, ctl, "#!/bin/sh\necho \"ran $* for $VM_ID\"\n")
	if out, err := run(ctl); err != nil || strings.TrimSpace(out) != "ran vm truenas stop for 7" {
		t.Fatalf("the installer there: %v %q", err, out)
	}
}
