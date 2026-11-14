package provider

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// adopt sets, for an imported state, what its first apply compares the configuration with: the
// values the install records (status --json --values), for the keys the configuration gives — a
// recorded value it leaves out is kept, never emptied. A secret input is compared by key alone:
// one the install records is kept, one it does not is sent. Without the values (an installer
// made before Stowage 0.38.0, or one that cannot tell what is secret) every value given is sent.
func (u *update) adopt(ctx context.Context, st *installer.Status) {
	if !imported(u.state) || st == nil || st.Values == nil || st.Inputs == nil {
		return
	}
	v, a := st.Values, *u.state
	// A map the configuration leaves out stays state's (null): nothing of it is compared.
	pick := func(plan, was types.Map, rec map[string]string) types.Map {
		if plan.IsNull() {
			return was
		}
		given := stringMap(ctx, plan, u.diags)
		out := map[string]attr.Value{}
		for k := range given {
			if r, ok := rec[k]; ok {
				out[k] = types.StringValue(r)
			}
		}
		return types.MapValueMust(types.StringType, out)
	}
	a.Inputs = pick(u.plan.Inputs, u.state.Inputs, v.Inputs)
	a.Domains = pick(u.plan.Domains, u.state.Domains, v.Domains)
	if !u.plan.Ports.IsNull() {
		ports := map[string]attr.Value{}
		for k := range portMap(ctx, u.plan.Ports, u.diags) {
			if p, ok := v.Ports[k]; ok {
				ports[k] = types.Int64Value(p)
			}
		}
		a.Ports = types.MapValueMust(types.Int64Type, ports)
	}
	// The mode as recorded ("none": loopback); none recorded (no front, or not told) is not
	// compared: the plan's stands.
	switch v.TLS {
	case "":
		a.TLS = u.plan.TLS
	case "none":
		a.TLS = types.StringNull()
	default:
		a.TLS = types.StringValue(v.TLS)
	}
	a.SecretInputsWOVersion = u.plan.SecretInputsWOVersion
	u.adopted, u.recorded = &a, *st.Inputs
	// Loopback and a TLS mode are each other's new install: no re-run moves between them, so
	// state would record a mode the install does not serve.
	switch rec, given := known(a.TLS), known(u.plan.TLS); {
	case v.TLS == "" || u.plan.TLS.IsUnknown() || rec == given:
	case rec:
		u.diags.AddAttributeError(path.Root("tls"), "the install serves a TLS mode",
			fmt.Sprintf("The install serves TLS mode %s and the configuration leaves tls out (loopback mode): give tls = %q. Moving to loopback mode is a new install.", v.TLS, v.TLS))
	default:
		u.diags.AddAttributeError(path.Root("tls"), "the install serves loopback mode",
			fmt.Sprintf("The install serves loopback mode and the configuration gives tls = %q: leave tls out. Moving to a TLS mode is a new install.", u.plan.TLS.ValueString()))
	}
}

// releaseChange is whether the plan changes the release beside the take-over: no, yes, or not
// known at plan (the fetch a certain re-run skips, of an install recording no installer).
type releaseChange int

const (
	noReleaseChange releaseChange = iota
	releaseChanges
	releaseMayChange
)

func releaseChangeOf(changes bool) releaseChange {
	if changes {
		return releaseChanges
	}
	return noReleaseChange
}

// adoptionReport is the plan's word on an imported install's first apply, from the split adopt
// left: what the re-run changes of the install's values and what the upgrade gives (the keys
// alone: a value may be private), or nothing; then the release change. guessed: the split was made
// without the new release (a certain re-run skips its fetch), so what the running release does not
// declare — an input, a secret input, a route's port — is told apart: the new release's upgrade
// gives what it declares, the re-run refuses the rest.
func adoptionReport(diags *diag.Diagnostics, u *update, rerun, up installer.Change, change releaseChange, running *installer.Describe, guessed bool) {
	if u.adopted == nil {
		diags.AddWarning("Taking over the install: its values are not known",
			"The installer the install runs predates `status --json --values` (Stowage 0.38.0), or cannot read the release it runs: the first apply re-runs the install with every value the configuration gives.")
		return
	}
	var changes, undeclared []string
	add := func(declared bool, what string) {
		if guessed && change != noReleaseChange && !declared {
			undeclared = append(undeclared, what)
		} else {
			changes = append(changes, what)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(rerun.Inputs)) {
		add(running.Declares(k), "input "+k)
	}
	for _, k := range slices.Sorted(maps.Keys(rerun.SecretInputs)) {
		add(running.Declares(k), "secret input "+k+" (the install records none)")
	}
	if rerun.TLS != "" {
		changes = append(changes, "tls "+rerun.TLS)
	}
	for _, k := range slices.Sorted(maps.Keys(rerun.Domains)) {
		changes = append(changes, "domain of "+k)
	}
	for _, k := range slices.Sorted(maps.Keys(rerun.Ports)) {
		add(running.HasRoute(k) || hasL4Route(running, k), "port of "+k)
	}
	var upgrade []string
	for _, k := range slices.Sorted(maps.Keys(up.Inputs)) {
		upgrade = append(upgrade, "input "+k)
	}
	for _, k := range slices.Sorted(maps.Keys(up.SecretInputs)) {
		upgrade = append(upgrade, "secret input "+k)
	}
	for _, k := range slices.Sorted(maps.Keys(up.Domains)) {
		upgrade = append(upgrade, "domain of "+k)
	}
	for _, k := range slices.Sorted(maps.Keys(up.Ports)) {
		upgrade = append(upgrade, "port of "+k)
	}
	var b strings.Builder
	if len(changes) == 0 {
		b.WriteString("The configuration's inputs, tls, domains and ports that the running release takes match what the install records: the first apply re-runs nothing for them.")
	} else {
		b.WriteString("The first apply re-runs the install with what differs from what it records: " + strings.Join(changes, ", ") + ".")
	}
	b.WriteString(map[releaseChange]string{
		noReleaseChange:  " The plan changes no release.",
		releaseChanges:   " The release change the plan makes runs after it, as planned.",
		releaseMayChange: " If the plan changes the release (the install records no installer to tell), that change runs after it.",
	}[change])
	if len(upgrade) > 0 {
		b.WriteString(" The upgrade gives what only the new release declares: " + strings.Join(upgrade, ", ") + ".")
	}
	if len(undeclared) > 0 {
		b.WriteString(" The running release does not declare " + strings.Join(undeclared, ", ") + ": the new release's upgrade gives what it declares, the re-run refuses the rest.")
	}
	b.WriteString(" Secret inputs the install records are kept, not compared: bump secret_inputs_wo_version later to send them.")
	summary := "Taking over the install: its values differ"
	if len(changes)+len(upgrade)+len(undeclared) == 0 {
		summary = "Taking over the install: its values match"
	}
	diags.AddWarning(summary, b.String())
}

// refuseNewRouteDomains refuses, beside a release change, a domain the re-run of the running
// release would be given for a route it does not have: its domain is the new release's domain
// input, given in inputs with the upgrade (as a plan refuses for a state not imported).
func refuseNewRouteDomains(diags *diag.Diagnostics, rerun installer.Change, running *installer.Describe) {
	for _, k := range slices.Sorted(maps.Keys(rerun.Domains)) {
		if !running.HasRoute(k) && !hasL4Route(running, k) {
			diags.AddAttributeError(path.Root("domains").AtMapKey(k), "a domain added with a release change",
				fmt.Sprintf("domains.%s names a route the running release does not have, while installer changes: a route the new release adds takes its domain from its domain input, given in inputs with the upgrade. Add domains.%s in another apply.", k, k))
		}
	}
}

// hasL4Route says whether d declares the L4 route name.
func hasL4Route(d *installer.Describe, name string) bool {
	return slices.ContainsFunc(d.L4Routes, func(r installer.L4Route) bool { return r.Name == name })
}
