package installer

import (
	"context"
	"fmt"
	"strings"
)

// RotationsMinStowage is the first Stowage release whose describe --json lists each secret's
// rotate way per shape.
const RotationsMinStowage = "0.46.0"

// The shapes, as describe --json names them.
const (
	ShapeDocker     = "docker"
	ShapeKubernetes = "kubernetes"
	ShapeVM         = "kairos"
)

// The ways `secrets rotate KEY` changes a secret on a shape, as describe --json names them.
const (
	RotatePasses  = "passes"  // three passes: --stage, the swap (no flag), --finish
	RotateReplace = "replace" // one pass, confirmed by --yes
	RotateNone    = "none"    // refused
)

// DescribedSecret is a generated secret as describe --json lists it: never its value.
type DescribedSecret struct {
	Key      string            `json:"key"`
	Rotate   map[string]string `json:"rotate"`
	Settable map[string]bool   `json:"settable"`
	AsFile   bool              `json:"as_file"`
}

// RotateWays are the rotate ways of the release's secrets on shape, by key; nil when describe
// lists no secrets (an installer made before RotationsMinStowage). A secret describe names no way
// for on shape is "" (it rotates in none there).
func (d *Describe) RotateWays(shape string) map[string]string {
	if d.Secrets == nil {
		return nil
	}
	ways := make(map[string]string, len(d.Secrets))
	for _, s := range d.Secrets {
		ways[s.Key] = s.Rotate[shape]
	}
	return ways
}

// rotationPasses are the flags of each pass `secrets rotate KEY` takes in way, in order: one
// pass confirmed without a terminal for replace, the three for passes.
func rotationPasses(way string) ([][]string, error) {
	switch way {
	case RotateReplace:
		return [][]string{{"--yes", "--no-input"}}, nil
	case RotatePasses:
		return [][]string{{"--stage"}, nil, {"--finish"}}, nil
	}
	return nil, fmt.Errorf("way %q is not one this provider runs", way)
}

// passName names a pass by its flags.
func passName(flags []string) string {
	if len(flags) == 0 {
		return "the swap"
	}
	return "`" + flags[0] + "`"
}

// RotationStoppedError is a rotation by passes whose pass Pass (its index, from 0) failed: the
// secret may be mid-rotation on the target. Running that pass again is safe — a pass whose
// change the installer recorded already brings the stack up with it — so the rotation resumes
// there.
type RotationStoppedError struct {
	Key  string
	Pass int
	Name string // the pass's name
	Err  error  // the pass's own error
}

func (e *RotationStoppedError) Error() string {
	return fmt.Sprintf("%v\nThe rotation of %s stopped at %s: the next apply runs it again, then the passes after it.", e.Err, e.Key, e.Name)
}

func (e *RotationStoppedError) Unwrap() error { return e.Err }

// RotationPasses is the number of passes key's rotation in way has.
func RotationPasses(way string) int {
	p, _ := rotationPasses(way)
	return len(p)
}

// rotate runs the passes of key's rotation in way by pass (given the pass's flags), from the
// pass of index from, stopping at the first that fails: a rotation by passes stopped so is a
// *RotationStoppedError naming that pass.
func rotate(ctx context.Context, key, way string, from int, pass func(ctx context.Context, flags []string) error) error {
	passes, err := rotationPasses(way)
	if err != nil {
		return fmt.Errorf("secrets rotate %s: %w", key, err)
	}
	if from < 0 || from >= len(passes) {
		from = 0
	}
	for i := from; i < len(passes); i++ {
		if err := pass(ctx, passes[i]); err != nil {
			if way != RotatePasses {
				return err
			}
			return &RotationStoppedError{Key: key, Pass: i, Name: passName(passes[i]), Err: err}
		}
	}
	return nil
}

// plainOutcome maps a verb run without --json: exit 0 done, 3 refused (its reason the last line
// of standard error), any other exit failed.
func plainOutcome(verb string, stderr []byte, exit int) error {
	switch exit {
	case 0:
		return nil
	case 3:
		return &RefusedError{Verb: verb, Refusal: Refusal{Reason: lastLine(stderr)}}
	}
	return &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr)}
}

// rotateArgs is `secrets rotate KEY` with a pass's flags.
func rotateArgs(key string, flags []string) []string {
	return append([]string{"secrets", "rotate", key}, flags...)
}

// RotateSecret runs key's rotation in way with the installer the install keeps (`<short>-stow
// secrets rotate KEY … --dir`), each pass bringing the stack up with it.
func (d Docker) RotateSecret(ctx context.Context, key, way string, from int) error {
	return rotate(ctx, key, way, from, func(ctx context.Context, flags []string) error {
		verb := "secrets rotate " + strings.TrimSpace(key+" "+strings.Join(flags, " "))
		_, stderr, exit, err := d.stow(ctx, nil, true, rotateArgs(key, flags)...)
		if err != nil {
			return fmt.Errorf("%s: %w", verb, err)
		}
		return plainOutcome(verb, stderr, exit)
	})
}

// RotateSecret runs key's rotation in way on the VM (`sudo -n <short>-stow secrets rotate KEY …`),
// a job of its host daemon.
func (v VM) RotateSecret(ctx context.Context, key, way string, from int) error {
	return rotate(ctx, key, way, from, func(ctx context.Context, flags []string) error {
		return v.plain(ctx, "secrets rotate "+strings.TrimSpace(key+" "+strings.Join(flags, " ")), "", rotateArgs(key, flags)...)
	})
}

// RotateSecret runs key's rotation in way on the release, by the fetched installer (`k8s secrets
// rotate KEY … --namespace … --release …`), each pass restarting the workloads that read it.
// The verb reads no registry (`k8s status` neither): it takes no --registry-ca.
func (k K8s) RotateSecret(ctx context.Context, key, way string, from int) error {
	return rotate(ctx, key, way, from, func(ctx context.Context, flags []string) error {
		verb := "k8s secrets rotate " + strings.TrimSpace(key+" "+strings.Join(flags, " "))
		_, stderr, exit, err := k.run(ctx, nil, k.k8sRotateArgs(key, flags)...)
		if err != nil {
			return fmt.Errorf("%s: %w", verb, err)
		}
		return plainOutcome(verb, stderr, exit)
	})
}

// k8sRotateArgs is `k8s secrets rotate KEY` with a pass's flags, then the release's flags.
func (k K8s) k8sRotateArgs(key string, flags []string) []string {
	return append(append([]string{"k8s"}, rotateArgs(key, flags)...), k.scope()...)
}
