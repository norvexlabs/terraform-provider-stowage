package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// KitMinStowage is the first Stowage release whose installers issue a kit as JSON on Docker and
// Kubernetes (`kit create --json --request`) and list their handoffs in describe.
const KitMinStowage = "0.44.0"

// Handoff is a kit the release issues, as describe lists it (Stowage >= KitMinStowage).
type Handoff struct {
	Name string `json:"name"`
	// For is the project whose install takes the kit.
	For string `json:"for"`
	// Instance is the question naming the instance; "" when the handoff's name is the default.
	Instance string       `json:"instance"`
	Ask      []HandoffAsk `json:"ask"`
	Gives    []string     `json:"gives"`
	Check    bool         `json:"check"`
}

// HandoffAsk is a question of a handoff.
type HandoffAsk struct {
	Key    string `json:"key"`
	Prompt string `json:"prompt"`
	Secret bool   `json:"secret"`
}

// Handoff is the handoff name the release issues; nil when it issues none of that name.
func (d *Describe) Handoff(name string) *Handoff {
	for i := range d.Handoffs {
		if d.Handoffs[i].Name == name {
			return &d.Handoffs[i]
		}
	}
	return nil
}

// KitRequest is what `kit create --json` reads: the instance ("" for the handoff's name), the
// answers to what the handoff asks (secret ones too), and how long the kit is good for, in
// nanoseconds (0 for the installer's default).
type KitRequest struct {
	Instance string            `json:"instance,omitempty"`
	Answers  map[string]string `json:"answers"`
	Expires  int64             `json:"expires,omitempty"`
}

// IssuedKit is a kit `kit create --json` issued: the armored kit and its code. Neither is ever
// logged, written to a file of this provider's, or put in an error.
type IssuedKit struct {
	Kit  string `json:"kit"`
	Code string `json:"code"`
}

// ErrKitLost is said with a failure after a kit's run may have started: what it issued is gone.
var ErrKitLost = errors.New("if the handoff's run started, what it issued is lost: the project's script decides whether it can be issued again")

// parseIssuedKit reads `kit create --json`'s answer: one object holding a kit and a code. Its
// errors never quote the answer.
func parseIssuedKit(b []byte) (*IssuedKit, error) {
	var k IssuedKit
	if err := decodeOne("kit create --json", b, &k); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return nil, fmt.Errorf("kit create --json: the answer is not one JSON object (at byte %d)", syn.Offset)
		}
		return nil, errors.New("kit create --json: the answer does not read as a kit and a code")
	}
	if k.Kit == "" || k.Code == "" {
		return nil, errors.New("kit create --json: the answer holds no kit or no code")
	}
	return &k, nil
}

// kitOutcome is a kit create's issued kit, or its failure: a refusal before the run (exit 3, or
// a flag the installer does not know: one made before KitMinStowage), or a failure after which
// what the run issued is lost.
func kitOutcome(verb string, stdout, stderr []byte, exit int) (*IssuedKit, error) {
	if exit == 0 {
		k, err := parseIssuedKit(stdout)
		if err != nil {
			return nil, fmt.Errorf("%w; %w", err, ErrKitLost)
		}
		return k, nil
	}
	if unknownFlag(stderr, exit, "--json", "--request") {
		return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr),
			Cause: fmt.Errorf("the installer issues no kit as JSON: use one made by Stowage %s or later", KitMinStowage)}
	}
	if exit == 3 {
		return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr), Cause: errors.New("refused before the run: nothing was issued")}
	}
	return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr), Cause: ErrKitLost}
}

// kitRequestFile is req as the file --request reads.
func kitRequestFile(req KitRequest) (map[string]string, error) {
	if req.Answers == nil {
		req.Answers = map[string]string{}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return map[string]string{"request": string(b)}, nil
}

// KitCheck is `kit check`'s verdict: 0 proven, 1 not proven, 2 unknown, as the installer says it.
type KitCheck int

// The verdicts of `kit check`.
const (
	KitProven    KitCheck = 0
	KitNotProven KitCheck = 1
	KitUnknown   KitCheck = 2
)

// kitVerdict reads `kit check`'s answer: its exit 0 and 1 are proven and not proven only with the
// verdict line it prints (`HANDOFF INSTANCE: proven`, `…: not proven (…)`), as the program that
// runs it (sudo, ssh, the script finding the installer) fails with 1 too; 2 is unknown. Any other
// answer is no answer, an error naming it.
func kitVerdict(verb, handoff, instance string, stdout, stderr []byte, exit int) (KitCheck, error) {
	verdict := ""
	for _, l := range strings.Split(string(stdout), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), handoff+" "+instance+": "); ok {
			verdict = v
		}
	}
	switch {
	case exit == 0 && verdict == "proven":
		return KitProven, nil
	case exit == 1 && strings.HasPrefix(verdict, "not proven"):
		return KitNotProven, nil
	case exit == 2:
		return KitUnknown, nil
	}
	return KitUnknown, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr)}
}

// KitCreate runs the kept installer's `kit create HANDOFF --json` on the install, the request a
// file (--request): standard input carries the run's files.
func (d Docker) KitCreate(ctx context.Context, handoff string, req KitRequest) (*IssuedKit, error) {
	files, err := kitRequestFile(req)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := d.stow(ctx, files, true, "kit", "create", handoff, "--json")
	if err != nil {
		return nil, fmt.Errorf("kit create: %w; %w", err, ErrKitLost)
	}
	return kitOutcome("kit create", stdout, stderr, exit)
}

// KitCheck runs the kept installer's `kit check HANDOFF INSTANCE` on the install.
func (d Docker) KitCheck(ctx context.Context, handoff, instance string) (KitCheck, error) {
	stdout, stderr, exit, err := d.stow(ctx, nil, true, "kit", "check", handoff, instance)
	if err != nil {
		return KitUnknown, fmt.Errorf("kit check: %w", err)
	}
	return kitVerdict("kit check", handoff, instance, stdout, stderr, exit)
}

// KitCreate runs the fetched installer's `k8s kit create HANDOFF --json` on the release, the
// request a file (--request), with the registry CA when given.
func (k K8s) KitCreate(ctx context.Context, handoff string, req KitRequest, registryCA string) (*IssuedKit, error) {
	files, err := kitRequestFile(req)
	if err != nil {
		return nil, err
	}
	if registryCA != "" {
		files["registry-ca"] = registryCA
	}
	stdout, stderr, exit, err := k.run(ctx, files, k.kitVerb("create", handoff, "--json")...)
	if err != nil {
		return nil, fmt.Errorf("k8s kit create: %w; %w", err, ErrKitLost)
	}
	return kitOutcome("k8s kit create", stdout, stderr, exit)
}

// KitCheck runs the fetched installer's `k8s kit check HANDOFF INSTANCE` on the release.
func (k K8s) KitCheck(ctx context.Context, handoff, instance, registryCA string) (KitCheck, error) {
	var files map[string]string
	if registryCA != "" {
		files = map[string]string{"registry-ca": registryCA}
	}
	stdout, stderr, exit, err := k.run(ctx, files, k.kitVerb("check", handoff, instance)...)
	if err != nil {
		return KitUnknown, fmt.Errorf("k8s kit check: %w", err)
	}
	return kitVerdict("k8s kit check", handoff, instance, stdout, stderr, exit)
}

// kitVerb is `k8s kit <verb> args…`, then the release's flags.
func (k K8s) kitVerb(verb string, args ...string) []string {
	return append(append([]string{"k8s", "kit", verb}, args...), k.scope()...)
}

// KitCreate runs the VM's `sudo -n <short>-stow kit create HANDOFF --json`, the request on
// standard input: never logged, as the answer holds the kit and its code.
func (v VM) KitCreate(ctx context.Context, handoff string, req KitRequest) (*IssuedKit, error) {
	ctl, err := v.ctl()
	if err != nil {
		return nil, err
	}
	if req.Answers == nil {
		req.Answers = map[string]string{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := v.Runner.Run(ctx, []string{"sudo", "-n", ctl, "kit", "create", handoff, "--json"}, body)
	if err != nil {
		return nil, fmt.Errorf("kit create: %w; %w", err, ErrKitLost)
	}
	return kitOutcome("kit create", stdout, stderr, exit)
}

// KitCheck runs the VM's `sudo -n <short>-stow kit check HANDOFF INSTANCE`; ssh failing (255) is
// no answer.
func (v VM) KitCheck(ctx context.Context, handoff, instance string) (KitCheck, error) {
	ctl, err := v.ctl()
	if err != nil {
		return KitUnknown, err
	}
	argv := []string{"sudo", "-n", ctl, "kit", "check", handoff, instance}
	stdout, stderr, exit, err := v.Runner.Run(ctx, argv, nil)
	logRun(ctx, argv, stdout, stderr, true, exit, err)
	if err != nil {
		return KitUnknown, fmt.Errorf("kit check: %w", err)
	}
	return kitVerdict("kit check", handoff, instance, stdout, stderr, exit)
}

// takeFiles are a kit and its code as the files --kit and --kit-code-file read: a take with no
// kit is refused here, one without its code by kitFiles.
func takeFiles(kit, code string) (map[string]string, error) {
	if kit == "" {
		return nil, errors.New("no kit to take")
	}
	files := map[string]string{}
	return files, kitFiles(files, kit, code)
}

// TakeKit runs the kept installer again on the install, given a kit (`<short>-stow --kit FILE
// --kit-code-file FILE --no-input --json --dir`): the re-run takes what it holds as --given's.
// The kit and its code are files on standard input, never on the command line.
func (d Docker) TakeKit(ctx context.Context, kit, code string) (*Result, error) {
	files, err := takeFiles(kit, code)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := d.stow(ctx, files, true, "--no-input", "--json")
	if err != nil {
		return nil, fmt.Errorf("re-run --kit: %w", err)
	}
	return Outcome("re-run --kit", stdout, stderr, exit)
}

// TakeKit applies the release again given a kit (`k8s upgrade --kit FILE --kit-code-file FILE`,
// by the installer of the release it runs), as Give does a --given file.
func (k K8s) TakeKit(ctx context.Context, kit, code, registryCA string) (*Result, error) {
	files, err := takeFiles(kit, code)
	if err != nil {
		return nil, err
	}
	if registryCA != "" {
		files["registry-ca"] = registryCA
	}
	args, login := k.pull(k.verb("upgrade", "--no-input", "--json"))
	stdout, stderr, exit, err := k.runLogin(ctx, login, k.chart(files), args...)
	if err != nil {
		return nil, fmt.Errorf("k8s upgrade --kit: %w", err)
	}
	return Outcome("k8s upgrade --kit", stdout, stderr, exit)
}

// TakeKit runs the VM's install again given a kit (`sudo -n <short>-stow install --kit FILE
// --kit-code-file FILE --no-input --json`).
func (v VM) TakeKit(ctx context.Context, kit, code string) (*Result, error) {
	if _, err := takeFiles(kit, code); err != nil {
		return nil, err
	}
	return v.Install(ctx, VMInstall{Kit: kit, KitCode: code})
}
