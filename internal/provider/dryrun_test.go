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

// The rollback the status names goes alone, as at apply (a port of a route the running
// release has is the re-run's); it runs without the inputs' variables, as the apply's; a
// first-install setting the install holds otherwise is the apply's error at plan; a port of an
// existing route beside an upgrade is told before anything is fetched; a release already
// running asks nothing.
func TestPlanDryRunRollbackAndReRuns(t *testing.T) {
	t.Parallel()
	statusPrev := `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable",` +
		`"previous":{"version":"2.0.0","revision":1,"installer":"` + image2 + `"},"inputs":["ADMIN_EMAIL"]}`
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8080)})}))
	web := func(p int64) tftypes.Value {
		return tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(p)})
	}
	h.fake.reset()
	h.fake.set("status", reply{stdout: statusPrev})
	got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"allow_rollback": boolean(true), "ports": web(8081)})).Diagnostics)
	if !strings.Contains(got, "a rollback goes alone") || len(h.fake.callsOf("dry-rollback")) != 0 {
		t.Fatalf("a rollback with a port: %q %q", got, h.fake.ops())
	}
	h.fake.reset()
	h.fake.set("status", reply{stdout: statusPrev})
	h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "allow_rollback": boolean(true), "ports": web(8080)}))
	if rb := h.fake.callsOf("dry-rollback"); len(rb) != 1 || !slices.Contains(rb[0].unset, "ADMIN_EMAIL") {
		t.Fatalf("the rollback's environment: %q", rb)
	}
	h.fake.reset()
	h.fake.set("status", reply{stdout: strings.Replace(statusPrev, `"inputs"`, `"address":"10.0.0.5","inputs"`, 1)})
	got = errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "ports": web(8080)})).Diagnostics)
	if !strings.Contains(got, "The install was made otherwise") {
		t.Fatalf("a held setting: %q", got)
	}
	h.fake.reset()
	h.fake.set("status", reply{stdout: strings.Replace(statusPrev, `"previous":{"version":"2.0.0","revision":1,"installer":"`+image2+`"}`, `"previous":null`, 1)})
	p := h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "ports": web(8081)}))
	if !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan") || slices.Contains(h.fake.ops(), "fetch") || slices.Contains(h.fake.ops(), "plan-dir") {
		t.Fatalf("a port of an existing route: %q %q", diagText(p.Diagnostics), h.fake.ops())
	}
	h.fake.reset()
	h.fake.set("status", reply{stdout: status2})
	p = h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "ports": web(8081)}))
	if d := diagText(p.Diagnostics); d != "" || slices.Contains(h.fake.ops(), "plan-dir") {
		t.Fatalf("a release already running: %q %q", d, h.fake.ops())
	}
}

// A rollback's dry run checks the inputs as the apply does before reaching the
// target (a secret input in inputs is refused, nothing run).
func TestPlanDryRunRollbackChecksTheInputs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable",` +
		`"previous":{"version":"2.0.0","revision":1,"installer":"` + image2 + `"},"inputs":["ADMIN_EMAIL"]}`})
	h.fake.set("kept-describe", reply{stdout: strings.Replace(describe1, `{"key":"ADMIN_EMAIL"}`, `{"key":"ADMIN_EMAIL","secret":true}`, 1)})
	got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "allow_rollback": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})).Diagnostics)
	if !strings.Contains(got, "secret input") || len(h.fake.callsOf("dry-rollback")) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	// The kept installer gone since status read it: no plan error, the apply says so.
	h.fake.reset()
	h.fake.set("kept-describe", reply{exit: 100, stderr: "stowage-provider: no install directory\n"})
	if got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "allow_rollback": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})).Diagnostics); got != "" {
		t.Fatalf("gone: %q", got)
	}
}

// Locally, a rollback's dry run refuses an input the run's environment sets, as the apply does.
func TestPlanDryRunRollbackRunEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128") // a variable the local run keeps
	h := newHarness(t, nil)
	local := func(extra map[string]tftypes.Value) tftypes.Value {
		vals := map[string]tftypes.Value{"docker": h.block("docker", map[string]tftypes.Value{"work_dir": str("/srv")})}
		for k, v := range extra {
			vals[k] = v
		}
		return h.dockerConfig(vals)
	}
	state := h.create(local(nil))
	h.fake.reset()
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable",` +
		`"previous":{"version":"2.0.0","revision":1,"installer":"` + image2 + `"},"inputs":[]}`})
	h.fake.set("kept-describe", reply{stdout: strings.Replace(describe1, `{"key":"LEGACY"}`, `{"key":"LEGACY"},{"key":"HTTPS_PROXY"}`, 1)})
	got := errorsOf(h.plan(state, local(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "allow_rollback": boolean(true),
		"inputs": strMap(map[string]string{"HTTPS_PROXY": "http://other:1"})})).Diagnostics)
	if !strings.Contains(got, "comes from the environment") || len(h.fake.callsOf("dry-rollback")) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// The kept installer is the one the status's short name names, as the apply finds it.
func TestPlanDryRunTakesTheStatusShortName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.reset()
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"short_name":"notes"`, `"short_name":"nt"`, 1)})
	h.fake.set("describe", reply{stdout: describe2})
	h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true)}))
	kd := h.fake.callsOf("kept-describe")
	if len(kd) != 1 || kd[0].argv[6] != "nt" {
		t.Fatalf("kept-describe %q", kd)
	}
}

// An input moved to secret_inputs_wo without a new version is the plan's error before the dry run
// (planMovedSecrets), which then never starts: nothing reaches the target. The dry run itself
// holds no check of it, as none can be reached.
func TestPlanDryRunAfterAMovedSecret(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"secret_inputs_wo": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})).Diagnostics)
	if strings.Count(got, "an input moved to secret_inputs_wo") != 1 || len(h.fake.ops()) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// The plan's check of a moved secret input holds on the VM and on Kubernetes too, the one check
// of it on every target.
func TestMovedSecretAtPlanOnEveryTarget(t *testing.T) {
	t.Parallel()
	vh, _ := newVMHarness(t)
	vstate := vh.create(vh.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})}))
	if got := errorsOf(vh.plan(vstate, vh.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil),
		"secret_inputs_wo": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})).Diagnostics); !strings.Contains(got, "an input moved to secret_inputs_wo") {
		t.Errorf("vm: %q", got)
	}
	kh := k8sHarness(t, nil)
	kh.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	kstate := kh.create(kh.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})}))
	if got := errorsOf(kh.plan(kstate, kh.k8sConfig(map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})).Diagnostics); !strings.Contains(got, "an input moved to secret_inputs_wo") {
		t.Errorf("k8s: %q", got)
	}
}

// plan_dry_run on a VM: a plan changing version runs `upgrade --version V --dry-run --json`
// over SSH, or `rollback --dry-run --json` going back to previous; a refusal is a plan error, the
// apply's own errors come first, and a plan that also re-runs the release is not dry-run.
func TestVMPlanDryRun(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	f.set("dry-upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.1.0","revision":1}}`})
	up := h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true)})
	if d := errorsOf(h.plan(state, up).Diagnostics); d != "" {
		t.Fatal(d)
	}
	dry := f.callsOf("dry-upgrade")
	if len(dry) != 1 || !slices.Equal(dry[0].argv, []string{"sudo", "-n", "notes-stow", "upgrade", "--version", "1.2.0", "--dry-run", "--json"}) ||
		len(f.callsOf("upgrade")) != 0 || len(f.callsOf("rollback")) != 0 {
		t.Fatalf("calls %q", f.ops())
	}
	// A refusal is the plan's error.
	f.reset()
	f.set("dry-upgrade", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"a job (backup) is running"}}`})
	if got := errorsOf(h.plan(state, up).Diagnostics); !strings.Contains(got, "a job (backup) is running") {
		t.Fatalf("a refusal: %q", got)
	}
	// With a re-run of the running release: a warning, no dry run.
	f.reset()
	both := h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})
	if p := h.plan(state, both); errorsOf(p.Diagnostics) != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan") || len(f.callsOf("dry-upgrade")) != 0 {
		t.Fatalf("with a re-run: %q, calls %q", diagText(p.Diagnostics), f.ops())
	}
	// Off, or version unchanged: nothing reaches the VM.
	f.reset()
	h.plan(state, h.vmConfig("1.2.0", nil))
	h.plan(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"plan_dry_run": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if ops := f.ops(); len(ops) != 0 {
		t.Fatalf("calls %q", ops)
	}
	// Going back to previous: the apply's refusal without allow_rollback, else rollback --dry-run.
	f.reset()
	withPrev := strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"previous":null`, `"previous":{"version":"1.0.0","revision":1}`, 1)
	f.set("status", reply{stdout: withPrev})
	back := h.vmConfig("1.0.0", map[string]tftypes.Value{"plan_dry_run": boolean(true)})
	if got := errorsOf(h.plan(state, back).Diagnostics); !strings.Contains(got, "a rollback needs allow_rollback") || len(f.callsOf("dry-rollback")) != 0 {
		t.Fatalf("without allow_rollback: %q, calls %q", got, f.ops())
	}
	f.set("dry-rollback", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"the database moved past 1.0.0","fix":"restore a backup"}}`})
	back = h.vmConfig("1.0.0", map[string]tftypes.Value{"plan_dry_run": boolean(true), "allow_rollback": boolean(true)})
	got := errorsOf(h.plan(state, back).Diagnostics)
	if rb := f.callsOf("dry-rollback"); !strings.Contains(got, "the database moved past 1.0.0") || len(rb) != 1 ||
		!slices.Equal(rb[0].argv, []string{"sudo", "-n", "notes-stow", "rollback", "--dry-run", "--json"}) {
		t.Fatalf("a rollback: %q, calls %q", got, f.ops())
	}
}

// plan_dry_run on Kubernetes: a plan changing installer reads the release and fetches the new
// installer as the apply does, then runs its `k8s upgrade --dry-run` given what the upgrade adds;
// a refusal is a plan error, and nothing is upgraded; a rollback runs `k8s rollback --dry-run`.
func TestK8sPlanDryRun(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.set("k8s-dry-upgrade", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"2.0.0 needs schema baseline 2","fix":"reset-db --new-baseline"}}`})
	cfg := h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "NEW_KEY": "added"})})
	got := errorsOf(h.plan(state, cfg).Diagnostics)
	dry := h.fake.callsOf("k8s-dry-upgrade")
	if !strings.Contains(got, "2.0.0 needs schema baseline 2") || !strings.Contains(got, "reset-db --new-baseline") || len(dry) != 1 ||
		flags(dry[0].argv)["--set"] != "NEW_KEY=added" || !strings.HasSuffix(dry[0].argv[4], "/next") || len(h.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	// With a re-run of the running release: a warning, no dry run.
	h.fake.reset()
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	both := h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	// Told before the new installer is fetched: one fetch, the running release's.
	if p := h.plan(state, both); errorsOf(p.Diagnostics) != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan") ||
		len(h.fake.callsOf("k8s-dry-upgrade")) != 0 || len(h.fake.callsOf("fetch")) != 1 {
		t.Fatalf("with a re-run: %q, calls %q", diagText(p.Diagnostics), h.fake.ops())
	}
	// Going back to previous: rollback --dry-run.
	h.fake.reset()
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"previous":null`, `"previous":{"version":"0.9.0","revision":1,"installer":"`+image2+`"}`, 1)})
	h.fake.set("k8s-dry-rollback", reply{stdout: `{"format":1,"result":"nothing"}`})
	back := h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true), "allow_rollback": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})
	if d := errorsOf(h.plan(state, back).Diagnostics); d != "" || len(h.fake.callsOf("k8s-dry-rollback")) != 1 || len(h.fake.callsOf("k8s-rollback")) != 0 {
		t.Fatalf("a rollback: %q, calls %q", d, h.fake.ops())
	}
	// Off: nothing runs.
	h.fake.reset()
	h.plan(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	if ops := h.fake.ops(); len(ops) != 0 {
		t.Fatalf("calls %q", ops)
	}
}

// A setting a run reads not known yet (the provider's registry from another resource): no dry
// run, nothing reaches the cluster.
func TestK8sPlanDryRunWaitsForTheProvidersSettings(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, map[string]tftypes.Value{"registry": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	p := h.plan(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	if ops := h.fake.ops(); len(ops) != 0 || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan yet") {
		t.Fatalf("%s, calls %q", diagText(p.Diagnostics), ops)
	}
}

// allow_rollback not known yet on Kubernetes: no dry run, a warning, no error, nothing reaches the
// cluster.
func TestK8sPlanDryRunWaitsForAllowRollback(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	p := h.plan(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"allow_rollback": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	if d := errorsOf(p.Diagnostics); d != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan yet") || len(h.fake.ops()) != 0 {
		t.Fatalf("allow_rollback not known: %s / %s, calls %q", d, diagText(p.Diagnostics), h.fake.ops())
	}
}

// short_name not known yet (its seed made again): no dry run, nothing reaches the VM.
func TestVMPlanDryRunWaitsForTheShortName(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	cfg := fill(h.typ, map[string]tftypes.Value{"plan_dry_run": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"}),
		"vm": h.vmBlock("1.2.0", map[string]tftypes.Value{"short_name": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})})
	p := h.plan(state, cfg)
	if d := errorsOf(p.Diagnostics); d != "" || len(f.ops()) != 0 || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan yet") {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), f.ops())
	}
}

// allow_rollback not known yet: no dry run, a warning, no error (the apply knows it).
func TestVMPlanDryRunWaitsForAllowRollback(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	withPrev := strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"previous":null`, `"previous":{"version":"1.0.0","revision":1}`, 1)
	f.set("status", reply{stdout: withPrev})
	p := h.plan(state, h.vmConfig("1.0.0", map[string]tftypes.Value{"plan_dry_run": boolean(true), "allow_rollback": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)}))
	if d := errorsOf(p.Diagnostics); d != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan yet") || len(f.ops()) != 0 {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), f.ops())
	}
}

// An upgrade whose plan also gives what only the new release declares: the dry run covers the
// upgrade, and a warning says the re-run after its reboot is not checked.
func TestVMPlanDryRunWarnsOfTheReRunAfter(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	f.set("dry-upgrade", reply{stdout: `{"format":1,"result":"nothing"}`})
	p := h.plan(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "NEW_KEY": "added"})}))
	if d := errorsOf(p.Diagnostics); d != "" || len(f.callsOf("dry-upgrade")) != 1 || !strings.Contains(diagText(p.Diagnostics), "The dry run covers the upgrade alone") {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), f.ops())
	}
}

// State without the VM's own key: the dry run runs, with a warning that the apply reads the key
// first and refuses the change when it cannot.
func TestVMPlanDryRunWithoutTheOwnKey(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.then("retire", reply{exit: 1, stderr: "sudo: a password is required"})
	f.then("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
	config := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" || vmAttr(t, h, a.NewState, "host_key_line") != "<null>" {
		t.Fatalf("%s %s", d, vmAttr(t, h, a.NewState, "host_key_line"))
	}
	f.reset()
	f.set("dry-upgrade", reply{stdout: `{"format":1,"result":"nothing"}`})
	p := h.plan(a.NewState, h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true)}))
	if d := errorsOf(p.Diagnostics); d != "" || len(f.callsOf("dry-upgrade")) != 1 || !strings.Contains(diagText(p.Diagnostics), "The VM's own host key is not known yet") {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), f.ops())
	}
}

// A rollback (state records previous) with allow_rollback not known yet: neither the plan's
// rollback check nor the dry run decides; a warning, no error.
func TestVMRollbackPlanWithAllowRollbackUnknown(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	withPrev := strings.Replace(vmStatusJSON("1.2.0", "boot-1", ""), `"previous":null`, `"previous":{"version":"1.1.0","revision":1}`, 1)
	f.set("describe", reply{stdout: describeOf("1.2.0", "")})
	f.set("status", reply{stdout: withPrev})
	state := h.create(h.vmConfig("1.2.0", nil))
	f.reset()
	p := h.plan(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"plan_dry_run": boolean(true), "allow_rollback": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)}))
	if d := errorsOf(p.Diagnostics); d != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan yet") || len(f.ops()) != 0 {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), f.ops())
	}
	// Known false still refuses at plan.
	if d := errorsOf(h.plan(state, h.vmConfig("1.1.0", nil)).Diagnostics); !strings.Contains(d, "a rollback needs allow_rollback") {
		t.Fatalf("allow_rollback false: %q", d)
	}
}

// Docker: a plan back to previous.installer with allow_rollback not known yet is no plan error.
func TestPlanRollbackWithAllowRollbackUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("status", reply{stdout: status2})
	up := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	h.fake.reset()
	p := h.plan(up.NewState, h.dockerConfig(map[string]tftypes.Value{"allow_rollback": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue), "plan_dry_run": boolean(true)}))
	if d := errorsOf(p.Diagnostics); d != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan yet") || len(h.fake.ops()) != 0 {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), h.fake.ops())
	}
}

// A VM whose OS carries an installer without --dry-run (Stowage before 0.38.0): the change is
// planned without a dry run, with a warning, never a plan error.
func TestVMPlanDryRunOnAnOlderOS(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	f.set("dry-upgrade", reply{exit: 1, stderr: "Error: unknown flag: --dry-run\n"})
	p := h.plan(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true)}))
	if d := errorsOf(p.Diagnostics); d != "" || !strings.Contains(diagText(p.Diagnostics), "without --dry-run (built with Stowage before 0.38.0)") {
		t.Fatalf("%s / %s", d, diagText(p.Diagnostics))
	}
	// What only the new release declares is still said to go unchecked.
	p = h.plan(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "NEW_KEY": "x"})}))
	if !strings.Contains(diagText(p.Diagnostics), "The dry run covers the upgrade alone") {
		t.Fatalf("older OS, a later re-run: %s", diagText(p.Diagnostics))
	}
	// Any other failure stays a plan error.
	f.set("dry-upgrade", reply{exit: 1, stderr: "Error: something else\n"})
	if d := errorsOf(h.plan(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true)})).Diagnostics); !strings.Contains(d, "dry run of the VM's upgrade") {
		t.Fatalf("another failure: %q", d)
	}
}

// The provider's kubernetes block with an exec argument not known yet (a cluster made in the same
// plan): no dry run, a warning, nothing reaches the cluster.
func TestK8sPlanDryRunWaitsForTheProvidersCluster(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	conn := h.ptyp.AttributeTypes["kubernetes"].(tftypes.Object)
	execT := conn.AttributeTypes["exec"].(tftypes.Object)
	args := tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("eks"), tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	h2 := newHarness(t, map[string]tftypes.Value{"kubernetes": fill(conn, map[string]tftypes.Value{"host": str("https://k8s.example:6443"),
		"exec": fill(execT, map[string]tftypes.Value{"api_version": str("client.authentication.k8s.io/v1"), "command": str("aws"), "args": args})})})
	p := h2.plan(state, h2.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	if d := errorsOf(p.Diagnostics); d != "" || !strings.Contains(diagText(p.Diagnostics), "No dry run of this plan yet") || len(h2.fake.ops()) != 0 {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), h2.fake.ops())
	}
}

// A value no dry run reads not known yet (deletion_protection from another resource): the dry run
// runs all the same.
func TestPlanDryRunIgnoresTheDestroysSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe2})
	p := h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "plan_dry_run": boolean(true),
		"deletion_protection": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	if d := errorsOf(p.Diagnostics); d != "" || len(h.fake.callsOf("dry-upgrade")) != 1 {
		t.Fatalf("%s / %s, calls %q", d, diagText(p.Diagnostics), h.fake.ops())
	}
}

// allow_rollback not known yet with an input change beside the rollback: no plan error (the apply
// decides between "needs allow_rollback" and "goes alone").
func TestRollbackAloneWaitsForAllowRollback(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	withPrev := strings.Replace(vmStatusJSON("1.2.0", "boot-1", ""), `"previous":null`, `"previous":{"version":"1.1.0","revision":1}`, 1)
	f.set("describe", reply{stdout: describeOf("1.2.0", "")})
	f.set("status", reply{stdout: withPrev})
	state := h.create(h.vmConfig("1.2.0", nil))
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"allow_rollback": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})
	if d := errorsOf(h.plan(state, cfg).Diagnostics); d != "" {
		t.Fatalf("VM: %s", d)
	}
	dh := newHarness(t, nil)
	ds := dh.create(dh.dockerConfig(nil))
	dh.fake.set("describe", reply{stdout: describe2})
	dh.fake.set("status", reply{stdout: status2})
	up := dh.update(ds, dh.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	if d := errorsOf(dh.plan(up.NewState, dh.dockerConfig(map[string]tftypes.Value{"allow_rollback": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})).Diagnostics); d != "" {
		t.Fatalf("Docker: %s", d)
	}
}
