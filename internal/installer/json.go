package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// ValidShortName says whether s can be the <short> of a kept <short>-stow: one path element, as
// a project's name.
func ValidShortName(s string) bool { return projectName.MatchString(s) }

// Format is the machine interface's version this provider reads.
const Format = 1

// MinStowage is the first Stowage release whose installers speak Format.
const MinStowage = "0.34.0"

// FormatError is an installer speaking a format this provider does not read.
type FormatError struct {
	Doc     string // "describe --json", "status --json", "install --json"…
	Format  int
	Missing bool // the output has no format
}

func (e *FormatError) Error() string {
	switch {
	case e.Missing:
		return fmt.Sprintf("%s: the output has no format: the installer predates the machine interface; use an installer made by Stowage %s or later", e.Doc, MinStowage)
	case e.Format > Format:
		return fmt.Sprintf("%s: format %d is newer than this provider reads (format %d): upgrade terraform-provider-stowage to a release that reads format %d", e.Doc, e.Format, Format, e.Format)
	default:
		return fmt.Sprintf("%s: format %d is unknown: this provider reads format %d, from installers made by Stowage %s or later", e.Doc, e.Format, Format, MinStowage)
	}
}

func checkFormat(doc string, f *int) error {
	if f == nil {
		return &FormatError{Doc: doc, Missing: true}
	}
	if *f != Format {
		return &FormatError{Doc: doc, Format: *f}
	}
	return nil
}

// Release is a release of the project: its version at a package revision, and on Docker and
// Kubernetes the installer image by digest that carries it.
type Release struct {
	Version   string `json:"version"`
	Revision  int64  `json:"revision"`
	Installer string `json:"installer,omitempty"`
}

func (r *Release) check(doc, field string) error {
	if r == nil {
		return nil
	}
	if r.Version == "" {
		return fmt.Errorf("%s: %s has no version", doc, field)
	}
	return nil
}

// Input is one input the release declares, as describe reports it.
type Input struct {
	Key      string          `json:"key"`
	Type     string          `json:"type,omitempty"`
	Secret   bool            `json:"secret,omitempty"`
	Required bool            `json:"required,omitempty"`
	Optional bool            `json:"optional,omitempty"`
	Shapes   []string        `json:"shapes,omitempty"`
	When     string          `json:"when,omitempty"`
	Advanced bool            `json:"advanced,omitempty"`
	Profile  string          `json:"profile,omitempty"`
	Prompt   string          `json:"prompt,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

// Describe is `describe --json`: what the installer's release offers.
type Describe struct {
	Format *int   `json:"format"`
	Name   string `json:"name"`
	// ShortName is the <short> of <short>-stow, the installer an install keeps in its directory.
	ShortName        string   `json:"short_name"`
	Release          Release  `json:"release"`
	Channel          string   `json:"channel"`
	Shapes           []string `json:"shapes"`
	Inputs           []Input  `json:"inputs"`
	FirstInstallOnly []string `json:"first_install_only"`
}

// Declares says whether the release declares the input key.
func (d *Describe) Declares(key string) bool {
	for _, i := range d.Inputs {
		if i.Key == key {
			return true
		}
	}
	return false
}

// Status is `status --json`: the fields common to every shape, and Docker's restore flags.
type Status struct {
	Format         *int     `json:"format"`
	Name           string   `json:"name"`
	Installed      *Release `json:"installed"`
	Channel        string   `json:"channel"`
	Installing     bool     `json:"installing"`
	InstallFailed  string   `json:"install_failed"`
	RestoreAwaited bool     `json:"restore_awaited"`
	VolumesSplit   bool     `json:"volumes_split"`
	Previous       *Release `json:"previous"`
	// Inputs are the recorded keys, never a value; sorted. Nil (absent, or null) is unknown: the
	// installer run does not carry the installed release, whose spec tells them — never "none".
	Inputs *Keys `json:"inputs"`
}

// Keys reads a list of keys given as a JSON array of strings or as an object's keys.
type Keys []string

// UnmarshalJSON implements json.Unmarshaler.
func (k *Keys) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		*k = sortedUnique(list)
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return errors.New("inputs: want an array of keys or an object")
	}
	list = make([]string, 0, len(obj))
	for key := range obj {
		list = append(list, key)
	}
	*k = sortedUnique(list)
	return nil
}

// Result is the object a verb ends with under --json.
type Result struct {
	Format  *int     `json:"format"`
	Result  string   `json:"result"`
	Release *Release `json:"release"`
	Refused *Refusal `json:"refused"`
}

// Refusal is why a rule said no before anything changed, and what to run.
type Refusal struct {
	Reason string `json:"reason"`
	Fix    string `json:"fix"`
}

// decodeOne decodes exactly one JSON value from b into v: trailing data is an error, unknown
// fields are not.
func decodeOne(doc string, b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", doc, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: data after the JSON object", doc)
	}
	return nil
}

// ParseDescribe reads `describe --json`.
func ParseDescribe(b []byte) (*Describe, error) {
	const doc = "describe --json"
	var d Describe
	if err := decodeOne(doc, b, &d); err != nil {
		return nil, err
	}
	if err := checkFormat(doc, d.Format); err != nil {
		return nil, err
	}
	if d.Name == "" {
		return nil, fmt.Errorf("%s: no name", doc)
	}
	if !ValidShortName(d.ShortName) {
		return nil, fmt.Errorf("%s: short_name %q is not an installer's short name", doc, d.ShortName)
	}
	if err := d.Release.check(doc, "release"); err != nil {
		return nil, err
	}
	return &d, nil
}

// ParseStatus reads `status --json`.
func ParseStatus(b []byte) (*Status, error) {
	const doc = "status --json"
	var s Status
	if err := decodeOne(doc, b, &s); err != nil {
		return nil, err
	}
	if err := checkFormat(doc, s.Format); err != nil {
		return nil, err
	}
	if s.Name == "" {
		return nil, fmt.Errorf("%s: no name", doc)
	}
	if err := s.Installed.check(doc, "installed"); err != nil {
		return nil, err
	}
	if err := s.Previous.check(doc, "previous"); err != nil {
		return nil, err
	}
	return &s, nil
}

// lastObject is the JSON object stdout ends with: the whole output, else the last one starting
// a line (a verb may print lines before it).
func lastObject(stdout []byte) []byte {
	b := bytes.TrimSpace(stdout)
	if json.Valid(b) {
		return b
	}
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '{' && (i == 0 || b[i-1] == '\n') && json.Valid(b[i:]) {
			return b[i:]
		}
	}
	return nil
}

// RefusedError is exit 3: a rule said no before anything changed.
type RefusedError struct {
	Verb string
	Refusal
}

func (e *RefusedError) Error() string {
	msg := fmt.Sprintf("%s refused: %s", e.Verb, e.Reason)
	if e.Fix != "" {
		msg += "\nFix: " + e.Fix
	}
	return msg
}

// FailedError is a run that failed: any exit but 0 and a well-formed 3.
type FailedError struct {
	Verb   string
	Exit   int
	Stderr string
	Cause  error // a malformed result, when that is why
}

func (e *FailedError) Error() string {
	msg := fmt.Sprintf("%s failed (exit %d)", e.Verb, e.Exit)
	if e.Exit == 255 {
		msg += ": ssh's own failure (connection, host key) or the program's"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	if e.Stderr != "" {
		msg += "\n" + e.Stderr
	}
	return msg
}

func (e *FailedError) Unwrap() error { return e.Cause }

// tail is the end of a program's error output, at most 4 KiB, for a diagnostic.
func tail(stderr []byte) string {
	const max = 4 << 10
	s := strings.TrimSpace(string(stderr))
	if len(s) > max {
		s = "…" + s[len(s)-max:]
	}
	return s
}

// Outcome maps a verb's run to its result: exit 0 and a result of "done" or "nothing" is
// success, exit 3 with a refusal is a *RefusedError, anything else a *FailedError (a format
// error is returned as itself).
func Outcome(verb string, stdout, stderr []byte, exit int) (*Result, error) {
	doc := verb + " --json"
	obj := lastObject(stdout)
	var r Result
	var perr error
	if obj == nil {
		perr = fmt.Errorf("%s: no JSON object in the output", doc)
	} else if perr = decodeOne(doc, obj, &r); perr == nil {
		if ferr := checkFormat(doc, r.Format); ferr != nil {
			return nil, ferr
		}
	}
	switch {
	case exit == 3 && perr == nil && r.Refused != nil && r.Refused.Reason != "":
		return nil, &RefusedError{Verb: verb, Refusal: *r.Refused}
	case exit == 3 && perr == nil:
		perr = fmt.Errorf("%s: exit 3 without a refusal", doc)
	case exit == 0 && perr == nil && r.Result != "done" && r.Result != "nothing":
		perr = fmt.Errorf("%s: result %q is neither done nor nothing", doc, r.Result)
	case exit == 0 && perr == nil:
		if err := r.Release.check(doc, "release"); err != nil {
			return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr), Cause: err}
		}
		return &r, nil
	}
	if exit != 0 && exit != 3 {
		perr = nil // the exit says it all; a missing object is expected
	}
	return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr), Cause: perr}
}

func sortedUnique(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}
