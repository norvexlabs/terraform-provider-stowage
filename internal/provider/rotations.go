package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// rotateWaysKey is the private state's key of the rotate ways the last describe read gave, on the
// resource's shape, and the release that describe was of: {"version", "revision", "ways": {KEY:
// way}}, ways null for a release whose describe lists no secrets. A plan reads them only while
// state runs that release.
const rotateWaysKey = "rotate_ways"

type recordedWays struct {
	Version  string            `json:"version"`
	Revision int64             `json:"revision"`
	Ways     map[string]string `json:"ways"`
}

// rotationPendingKey is the private state's key of the rotations a pass stopped: {KEY: the index
// of the pass that failed}, where the next rotation of KEY resumes.
const rotationPendingKey = "rotation_pending"

func getPending(ctx context.Context, p privateData) map[string]int {
	out := map[string]int{}
	if p == nil {
		return out
	}
	if b, d := p.GetKey(ctx, rotationPendingKey); !d.HasError() && len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func setPendingRotations(ctx context.Context, p privateData, pending map[string]int, diags *diag.Diagnostics) {
	if p == nil {
		return
	}
	var b []byte
	if len(pending) > 0 {
		b, _ = json.Marshal(pending) // a map of numbers always marshals
	}
	diags.Append(p.SetKey(ctx, rotationPendingKey, b)...)
}

// shapeOf is the shape describe names the resource's target by.
func shapeOf(m *installModel) string {
	switch targetKind(m) {
	case "kubernetes":
		return installer.ShapeKubernetes
	case "vm":
		return installer.ShapeVM
	}
	return installer.ShapeDocker
}

// setRotateWays keeps desc's rotate ways on shape as the ones the plan checks secret_rotations by.
func setRotateWays(ctx context.Context, p privateData, desc *installer.Describe, shape string, diags *diag.Diagnostics) {
	b, err := json.Marshal(recordedWays{Version: desc.Release.Version, Revision: desc.Release.Revision, Ways: desc.RotateWays(shape)})
	if err != nil || p == nil {
		return
	}
	diags.Append(p.SetKey(ctx, rotateWaysKey, b)...)
}

// getRotateWays is the rotate ways the last describe gave; false when none was kept (a state
// made before secret_rotations, or imported and never applied).
func getRotateWays(ctx context.Context, p privateData) (recordedWays, bool) {
	var w recordedWays
	if p == nil {
		return w, false
	}
	b, d := p.GetKey(ctx, rotateWaysKey)
	if d.HasError() || len(b) == 0 || json.Unmarshal(b, &w) != nil {
		return w, false
	}
	return w, true
}

// rotationNumbers is a known secret_rotations' known numbers, by key.
func rotationNumbers(m types.Map) map[string]int64 {
	out := map[string]int64{}
	if !known(m) {
		return out
	}
	for k, v := range m.Elements() {
		if n, ok := v.(types.Int64); ok && known(n) {
			out[k] = n.ValueInt64()
		}
	}
	return out
}

// rotationsDue are the keys of plan's secret_rotations whose number changed from state's, sorted:
// a key state does not hold is recorded, never rotated; a number not known yet counts as changed.
func rotationsDue(plan, state types.Map) []string {
	was := rotationNumbers(state)
	var due []string
	for _, k := range mapKeys(plan) {
		old, ok := was[k]
		if !ok {
			continue
		}
		if n, isInt := plan.Elements()[k].(types.Int64); isInt && known(n) && n.ValueInt64() == old {
			continue
		}
		due = append(due, k)
	}
	return due
}

// rotationsAdded are the keys of plan's secret_rotations state does not hold, sorted.
func rotationsAdded(plan, state types.Map) []string {
	was := rotationNumbers(state)
	var added []string
	for _, k := range mapKeys(plan) {
		if _, ok := was[k]; !ok {
			added = append(added, k)
		}
	}
	return added
}

// keptRotations is what the steps before the rotations save of plan's secret_rotations: a key
// due keeps state's number and a key added is left out, so a rotation not run (or a key not
// checked) stays in the next plan; a key taken out is dropped. Null when nothing is left.
func keptRotations(plan, state types.Map) types.Map {
	if !known(plan) {
		return types.MapNull(types.Int64Type)
	}
	if len(plan.Elements()) == 0 {
		return plan
	}
	was := rotationNumbers(state)
	elems := map[string]attr.Value{}
	for k, v := range plan.Elements() {
		old, ok := was[k]
		if !ok {
			continue
		}
		elems[k] = v
		if n, isInt := v.(types.Int64); !isInt || !known(n) || n.ValueInt64() != old {
			elems[k] = types.Int64Value(old)
		}
	}
	if len(elems) == 0 {
		return types.MapNull(types.Int64Type)
	}
	return types.MapValueMust(types.Int64Type, elems)
}

// withRotation is m's secret_rotations with key's number n.
func withRotation(m types.Map, key string, n attr.Value) types.Map {
	elems := map[string]attr.Value{key: n}
	if known(m) {
		for k, v := range m.Elements() {
			if k != key {
				elems[k] = v
			}
		}
	}
	return types.MapValueMust(types.Int64Type, elems)
}

// refuseRotations refuses each key of keys that ways (a release's, on shape) cannot rotate: ways
// nil is a release whose describe lists no secrets.
func refuseRotations(diags *diag.Diagnostics, keys []string, ways map[string]string, shape, release string) {
	if len(keys) == 0 {
		return
	}
	if ways == nil {
		diags.AddAttributeError(path.Root("secret_rotations"), "secret_rotations needs a newer release",
			fmt.Sprintf("The describe of %s does not say how its secrets rotate: secret_rotations needs a release built by Stowage %s or later. Take secret_rotations out, or move to such a release first.", release, installer.RotationsMinStowage))
		return
	}
	for _, k := range keys {
		p := path.Root("secret_rotations").AtMapKey(k)
		way, ok := ways[k]
		switch {
		case !ok:
			diags.AddAttributeError(p, "not a generated secret of the release",
				fmt.Sprintf("The describe of %s lists no generated secret %s (it lists %s): take %s out of secret_rotations.", release, k, listOrNone(mapKeysOf(ways)), k))
		case way == installer.RotateNone:
			diags.AddAttributeError(p, k+" does not rotate on "+shape,
				fmt.Sprintf("The describe of %s gives %s the rotate way none on %s: its rotation is refused there. Take %s out of secret_rotations.", release, k, shape, k))
		case way == "":
			diags.AddAttributeError(p, k+" does not rotate on "+shape,
				fmt.Sprintf("The describe of %s names no rotate way for %s on %s: its rotation is refused there. Take %s out of secret_rotations.", release, k, shape, k))
		case way != installer.RotatePasses && way != installer.RotateReplace:
			diags.AddAttributeError(p, k+" rotates in a way this provider does not run",
				fmt.Sprintf("The describe of %s gives %s the rotate way %q on %s, which this provider does not run: use a provider release that does, or take %s out of secret_rotations.", release, k, way, shape, k))
		}
	}
}

func mapKeysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func listOrNone(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

// planRotations refuses, at plan, a key of secret_rotations added or due that the release cannot
// rotate on the resource's shape, by the ways the last describe read gave: only when the plan
// keeps the release (a new one is checked by the apply, on its describe) and those ways are the
// release state runs (an upgrade, a rollback or a refresh reading another one leaves them out).
// A rotation a pass stopped whose key is no longer due is a warning: the target is mid-rotation.
func planRotations(ctx context.Context, p privateData, plan, state *installModel, diags *diag.Diagnostics) {
	due := rotationsDue(plan.SecretRotations, state.SecretRotations)
	for _, k := range mapKeysOfInt(getPending(ctx, p)) {
		if _, ok := plan.SecretRotations.Elements()[k]; ok && !slices.Contains(due, k) {
			diags.AddAttributeWarning(path.Root("secret_rotations").AtMapKey(k), "the rotation of "+k+" is not finished",
				fmt.Sprintf("An apply's rotation of %s stopped part-way on the target, its number not recorded: give secret_rotations.%s another number than state's, and the next apply resumes it where it stopped.", k, k))
		}
	}
	keys := append(due, rotationsAdded(plan.SecretRotations, state.SecretRotations)...)
	if len(keys) == 0 || !plan.Installer.Equal(state.Installer) || (plan.VM != nil && state.VM != nil && !plan.VM.Version.Equal(state.VM.Version)) {
		return
	}
	w, ok := getRotateWays(ctx, p)
	if !ok || w.Version != state.Version.ValueString() || w.Revision != state.Revision.ValueInt64() || !known(state.Version) {
		return
	}
	slices.Sort(keys)
	refuseRotations(diags, keys, w.Ways, shapeOf(plan), "the release the install runs ("+state.Version.ValueString()+")")
}

// createRotations refuses, before a first install, a key of secret_rotations the release (desc)
// cannot rotate on shape, and keeps its ways for the next plans: a create records the numbers,
// and rotates nothing.
func createRotations(ctx context.Context, p privateData, rotations types.Map, desc *installer.Describe, shape string, diags *diag.Diagnostics) {
	refuseRotations(diags, mapKeys(rotations), desc.RotateWays(shape), shape, desc.Name+" "+desc.Release.Version)
	setRotateWays(ctx, p, desc, shape, diags)
}

func mapKeysOfInt(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// forgetPending drops, before an update's steps, the resume point of a key taken out of
// secret_rotations or added (back): the rotation it marked is no longer the configuration's.
func forgetPending(ctx context.Context, p privateData, want, was types.Map, diags *diag.Diagnostics) {
	pending := getPending(ctx, p)
	n := len(pending)
	added := rotationsAdded(want, was)
	for k := range pending {
		if _, ok := want.Elements()[k]; !ok || slices.Contains(added, k) {
			delete(pending, k)
		}
	}
	if len(pending) != n {
		setPendingRotations(ctx, p, pending, diags)
	}
}

// rotator is what runs a target's rotations: the running release's describe, and a key's rotation.
type rotator struct {
	desc   *installer.Describe
	rotate func(ctx context.Context, key, way string, from int) error
	close  func()
}

// rotator reads the release m's install runs, on its target, and how to rotate its secrets.
func (r *installResource) rotator(ctx context.Context, m *installModel, diags *diag.Diagnostics) *rotator {
	switch targetKind(m) {
	case "docker":
		d, err := r.docker(m, m.ShortName.ValueString())
		if err != nil {
			diags.AddError("Cannot reach the Docker target", err.Error())
			return nil
		}
		desc, err := d.KeptDescribe(ctx)
		if err == nil && desc == nil {
			err = fmt.Errorf("%s/%s holds no install", d.WorkDir, d.Name)
		}
		if err != nil {
			addInstallerError(diags, "Reading the installer the install keeps", err)
			return nil
		}
		d.Short = desc.ShortName
		return &rotator{desc: desc, rotate: d.RotateSecret, close: func() {}}
	case "vm":
		v, err := r.vm(m)
		if err != nil {
			diags.AddError("Cannot reach the VM", err.Error())
			return nil
		}
		desc, err := v.Describe(ctx)
		if err != nil {
			addInstallerError(diags, "Reading the VM's installer", err)
			return nil
		}
		return &rotator{desc: desc, rotate: v.RotateSecret, close: func() {}}
	case "kubernetes":
		run := r.k8sStart(ctx, m, installName(m), diags)
		if run == nil {
			return nil
		}
		k, desc := run.fetch(ctx, "rotate", m.Installer.ValueString(), diags)
		if desc == nil {
			run.close()
			return nil
		}
		return &rotator{desc: desc, rotate: k.RotateSecret, close: run.close}
	}
	diags.AddError("No target", "The install names no target.")
	return nil
}

// rotateSecrets runs, after an update's other steps, the rotation of each key of want (the
// configuration's secret_rotations) due against was (state's before the update), on the release
// the install runs now, as its describe says: every key added or due checked first, the added
// recorded, then each due key rotated in key order, its number saved once it rotated. The first
// that fails stops the rest, which stay due. With nothing to rotate and secret_rotations set, an
// update whose kept ways are not the running release's (it changed the release, or none were
// kept) reads its describe for the ways the next plans check.
func (r *installResource) rotateSecrets(ctx context.Context, p privateData, want types.Map, was *installModel, get func(*installModel) bool, save func(*installModel) bool, diags *diag.Diagnostics) {
	due := rotationsDue(want, was.SecretRotations)
	added := rotationsAdded(want, was.SecretRotations)
	pending := getPending(ctx, p) // forgetPending pruned it before the update's other steps
	var m installModel
	if len(due) == 0 && len(added) == 0 {
		if len(mapKeys(want)) == 0 || !get(&m) {
			return
		}
		if w, ok := getRotateWays(ctx, p); ok && w.Version == m.Version.ValueString() && w.Revision == m.Revision.ValueInt64() {
			return
		}
		// Only the ways for the next plans, kept for another release or none: the update
		// succeeded, so a read that fails is a warning (the apply checks the ways anyway).
		var read diag.Diagnostics
		if rot := r.rotator(ctx, &m, &read); rot != nil {
			setRotateWays(ctx, p, rot.desc, shapeOf(&m), &read)
			rot.close()
		}
		// rotator and setRotateWays report errors alone (a read that failed).
		for _, d := range read {
			diags.AddWarning(d.Summary()+" (the release's rotate ways not read)", d.Detail())
		}
		return
	}
	if !get(&m) {
		return
	}
	rot := r.rotator(ctx, &m, diags)
	if rot == nil {
		return
	}
	defer rot.close()
	shape := shapeOf(&m)
	setRotateWays(ctx, p, rot.desc, shape, diags)
	keys := append(slices.Clone(due), added...)
	slices.Sort(keys)
	release := rot.desc.Name + " " + rot.desc.Release.Version
	if refuseRotations(diags, keys, rot.desc.RotateWays(shape), shape, release); diags.HasError() {
		return
	}
	ways := rot.desc.RotateWays(shape)
	if len(added) > 0 {
		for _, k := range added {
			m.SecretRotations = withRotation(m.SecretRotations, k, want.Elements()[k])
		}
		if !save(&m) {
			return
		}
	}
	for _, k := range due {
		from := 0
		if ways[k] == installer.RotatePasses {
			from = pending[k]
		} else {
			delete(pending, k) // a resume point is a rotation by passes
		}
		err := rot.rotate(ctx, k, ways[k], from)
		var stopped *installer.RotationStoppedError
		switch {
		case err == nil:
			delete(pending, k)
		case errors.As(err, &stopped) && stopped.Pass > 0:
			pending[k] = stopped.Pass
		case errors.As(err, &stopped):
			delete(pending, k) // the first pass: resuming is starting
		}
		setPendingRotations(ctx, p, pending, diags)
		if err != nil {
			rotationFailed(diags, k, from, stopped, err)
			return
		}
		m.SecretRotations = withRotation(m.SecretRotations, k, want.Elements()[k])
		if !save(&m) {
			return
		}
	}
}

// rotationFailed reports key's rotation that failed: a pass after the first stopped it, the
// passes before having changed the target, which the next apply resumes at; a resumed pass that
// failed may be a target moved on by hand, which taking the key out and adding it back forgets.
func rotationFailed(diags *diag.Diagnostics, key string, from int, stopped *installer.RotationStoppedError, err error) {
	if stopped == nil || (stopped.Pass == 0 && from == 0) {
		addInstallerError(diags, "Rotating "+key, err)
		return
	}
	detail := err.Error()
	if from > 0 && stopped.Pass == from {
		detail += fmt.Sprintf("\nThis apply resumed the rotation at %s, where an earlier one stopped. If the rotation was finished or moved on by hand since, take %s out of secret_rotations and apply, then give it back: the apply that adds it records its number and forgets where the rotation stopped.", passLabel(from), key)
	}
	diags.AddAttributeError(path.Root("secret_rotations").AtMapKey(key), "Rotating "+key+" stopped part-way", detail)
}

// passLabel names a rotation by passes' pass by its index.
func passLabel(i int) string {
	switch i {
	case 1:
		return "the swap"
	case 2:
		return "`--finish`"
	}
	return "`--stage`"
}
