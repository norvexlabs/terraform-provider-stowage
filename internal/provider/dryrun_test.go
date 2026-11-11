package provider

import (
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// plan_dry_run: a plan changing installer runs the new release's upgrade --dry-run in a private
// directory beside the install, removed after, given what the upgrade adds; a refusal is a plan
// error with the installer's reason and fix. Without it, or with no release change, nothing runs.
func TestPlanDryRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe2})
	cfg := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs":           strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "NEW_KEY": "k"}),
		"secret_inputs_wo": strMap(map[string]string{"NEW_TOKEN": "tok-" + secretVal}), "secret_inputs_wo_version": num(1)})
	if d := errorsOf(h.plan(state, cfg).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"status", "kept-describe", "plan-dir", "fetch", "describe", "dry-upgrade", "plan-rm"}) {
		t.Fatalf("calls %q", got)
	}
	dry := h.fake.callsOf("dry-upgrade")[0]
	if f := flags(dry.argv); f["--set"] != "NEW_KEY=k" || f["--dir"] != "/srv/notes" || dry.argv[4] != "/srv/.stowage-plan.abc123" || !contains(dry.argv, "./notes.sh") ||
		untarAll(t, dry.stdin)["answers"] != `NEW_TOKEN: "tok-`+secretVal+`"`+"\n" || strings.Contains(strings.Join(dry.argv, " "), secretVal) {
		t.Fatalf("dry run %q", dry.argv)
	}
	if fetch := h.fake.callsOf("fetch")[0]; fetch.argv[4] != "/srv/.stowage-plan.abc123" {
		t.Fatalf("fetched into %q", fetch.argv[4])
	}
	if rm := h.fake.callsOf("plan-rm")[0]; rm.argv[4] != "/srv/.stowage-plan.abc123" || rm.argv[5] != "/srv" {
		t.Fatalf("removed %q", rm.argv)
	}
	// A refusal is the plan's error; the directory is removed all the same.
	h.fake.reset()
	h.fake.set("dry-upgrade", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"2.0.0 needs schema baseline 2","fix":"reset-db --new-baseline"}}`})
	got := errorsOf(h.plan(state, cfg).Diagnostics)
	if !strings.Contains(got, "2.0.0 needs schema baseline 2") || !strings.Contains(got, "reset-db --new-baseline") || len(h.fake.callsOf("plan-rm")) != 1 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	// A secret input the new release adds, given in inputs: refused before it reaches the
	// target's command line.
	h.fake.reset()
	h.fake.set("dry-upgrade", reply{stdout: `{"format":1,"result":"nothing"}`})
	leak := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "NEW_TOKEN": "tok-" + secretVal})})
	if got := errorsOf(h.plan(state, leak).Diagnostics); !strings.Contains(got, "NEW_TOKEN is a secret input") || len(h.fake.callsOf("dry-upgrade")) != 0 {
		t.Fatalf("a secret in inputs: %q, calls %q", got, h.fake.ops())
	}
	// With a re-run of the running release: no dry run, a warning.
	h.fake.reset()
	both := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	if p := h.plan(state, both); errorsOf(p.Diagnostics) != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan") || len(h.fake.callsOf("dry-upgrade")) != 0 {
		t.Fatalf("with a re-run: %q, calls %q", diagText(p.Diagnostics), h.fake.ops())
	}
	// An input not known yet: no dry run, nothing reached.
	h.fake.reset()
	unknownIn := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{"ADMIN_EMAIL": str("a@example.com"), "NEW_KEY": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})})
	h.plan(state, unknownIn)
	if ops := h.fake.ops(); len(ops) != 0 {
		t.Fatalf("an unknown input reached the target: %q", ops)
	}
	// Off, or no release change: nothing reaches the target.
	h.fake.reset()
	off := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})
	h.plan(state, off)
	same := h.dockerConfig(map[string]tftypes.Value{"plan_dry_run": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	h.plan(state, same)
	if ops := h.fake.ops(); len(ops) != 0 {
		t.Fatalf("calls %q", ops)
	}
}

// A plan going back to previous.installer runs the kept installer's rollback --dry-run.
func TestPlanDryRunRollback(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("status", reply{stdout: status2})
	up := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	h.fake.reset()
	h.fake.set("dry-rollback", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"a rollback barrier after migration 4","fix":"restore a backup"}}`})
	got := errorsOf(h.plan(up.NewState, h.dockerConfig(map[string]tftypes.Value{"allow_rollback": boolean(true), "plan_dry_run": boolean(true)})).Diagnostics)
	if !strings.Contains(got, "a rollback barrier after migration 4") || !slices.Equal(h.fake.ops(), []string{"status", "kept-describe", "dry-rollback"}) {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// The status, not state, chooses the step, as the apply's: a release status names previous with
// allow_rollback off is the apply's refusal at plan; a re-run beside a release change is known
// before anything is fetched; the release installed under another digest is the apply's error.
func TestPlanDryRunAgreesWithTheApply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.reset()
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable",` +
		`"previous":{"version":"2.0.0","revision":1,"installer":"` + image2 + `"},"inputs":[]}`})
	got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true)})).Diagnostics)
	if !strings.Contains(got, "a rollback needs allow_rollback") || len(h.fake.callsOf("dry-rollback")) != 0 {
		t.Fatalf("status's previous without allow_rollback: %q %q", got, h.fake.ops())
	}
	h.fake.reset()
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","inputs":["ADMIN_EMAIL"]}`})
	p := h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan") || slices.Contains(h.fake.ops(), "fetch") {
		t.Fatalf("a re-run beside: %q %q", diagText(p.Diagnostics), h.fake.ops())
	}
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe1}) // image2 carries the release installed
	got = errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true)})).Diagnostics)
	if !strings.Contains(got, "installer is another digest of the release installed") || len(h.fake.callsOf("dry-upgrade")) != 0 {
		t.Fatalf("the same release: %q %q", got, h.fake.ops())
	}
}
