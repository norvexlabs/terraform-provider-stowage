package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// KeepLocalMinStowage is the first Stowage release whose `backup config` takes --keep-local.
const KeepLocalMinStowage = "0.47.0"

// BackupSettings is `backup config --json`: the install's backup settings, the rclone config's
// credentials never among them.
type BackupSettings struct {
	Configured      bool   `json:"configured"`
	Destination     string `json:"destination"`
	RcloneConfigSet bool   `json:"rclone_config_set"`
	// RcloneConfigFrom is "operator" for one saved over the operator's socket; another (unknown)
	// is never run until the operator saves one.
	RcloneConfigFrom string   `json:"rclone_config_from"`
	Recipients       []string `json:"recipients"`
	RetentionDays    int      `json:"retention_days"`
	Schedule         string   `json:"schedule"`
	Profile          string   `json:"profile"`
	KeepLocal        int      `json:"keep_local"`
	Locked           bool     `json:"locked"`
}

// RcloneConfigHeld reports whether the settings hold an rclone config the backups run with.
func (s *BackupSettings) RcloneConfigHeld() bool {
	return s.RcloneConfigSet && s.RcloneConfigFrom == "operator"
}

// BackupConfig is a change of the backup settings: each field set is given, the others keep the
// install's value.
type BackupConfig struct {
	Destination *string
	// RcloneConfig is sent as a file (--rclone-config), never on the command line; empty: not
	// given.
	RcloneConfig  string
	KeepLocal     *int
	Recipients    []string
	RetentionDays *int
	Schedule      *string
	Profile       *string
	Lock          *bool
}

// backupConfigArgs is `backup config … --json` for c, and the files it reads.
func backupConfigArgs(c BackupConfig) ([]string, map[string]string) {
	args := []string{"backup", "config"}
	if c.Destination != nil {
		args = append(args, "--destination", *c.Destination)
	}
	if c.KeepLocal != nil {
		args = append(args, "--keep-local", strconv.Itoa(*c.KeepLocal))
	}
	for _, r := range c.Recipients {
		args = append(args, "--recipient", r)
	}
	if c.RetentionDays != nil {
		args = append(args, "--retention-days", strconv.Itoa(*c.RetentionDays))
	}
	if c.Schedule != nil {
		args = append(args, "--schedule", *c.Schedule)
	}
	if c.Profile != nil {
		args = append(args, "--profile", *c.Profile)
	}
	if c.Lock != nil {
		if *c.Lock {
			args = append(args, "--lock")
		} else {
			args = append(args, "--unlock")
		}
	}
	var files map[string]string
	if c.RcloneConfig != "" {
		files = map[string]string{"rclone-config": c.RcloneConfig}
	}
	return append(args, "--json"), files
}

// backupSettingsFields are the fields every `backup config --json` answer holds.
var backupSettingsFields = []string{"configured", "recipients", "retention_days", "schedule", "profile", "locked"}

// parseBackupSettings reads `backup config --json`: one object holding every field of
// backupSettingsFields. Its errors never quote the answer.
func parseBackupSettings(b []byte) (*BackupSettings, error) {
	var s BackupSettings
	var fields map[string]json.RawMessage
	if decodeOne("backup config --json", b, &s) != nil || json.Unmarshal(b, &fields) != nil {
		return nil, errors.New("backup config --json: the answer does not read as the backup settings")
	}
	for _, f := range backupSettingsFields {
		if _, ok := fields[f]; !ok {
			return nil, fmt.Errorf("backup config --json: the answer holds no %s", f)
		}
	}
	return &s, nil
}

// backupOutcome is a backup config's settings, or its failure: a refusal (exit 3), a flag the
// installer does not know (--keep-local before KeepLocalMinStowage), or another failure.
func backupOutcome(stdout, stderr []byte, exit int) (*BackupSettings, error) {
	const verb = "backup config"
	if exit == 0 {
		return parseBackupSettings(stdout)
	}
	if unknownFlag(stderr, exit, "--keep-local") {
		return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr),
			Cause: fmt.Errorf("the installer keeps no backup on the host: keep_local needs Stowage %s or later", KeepLocalMinStowage)}
	}
	return nil, plainOutcome(verb, stderr, exit)
}

// ValidObject refuses an object that is no backup's name: empty, one that could read as a flag
// (a leading -), or one holding a space of any kind.
func ValidObject(object string) error {
	if object == "" || strings.HasPrefix(object, "-") || strings.IndexFunc(object, unicode.IsSpace) >= 0 {
		return fmt.Errorf("restore.object %q is not a backup's name (`backup list`): no leading -, no blank", object)
	}
	return nil
}

// restoreArgs is `restore OBJECT --yes [--unrecorded]`, the identity a file (--identity). An
// object that could read as a flag, or holds a blank, is refused: it is a backup's name.
func restoreArgs(object, identity string, unrecorded bool) ([]string, map[string]string, error) {
	if err := ValidObject(object); err != nil {
		return nil, nil, err
	}
	if identity == "" {
		return nil, nil, errors.New("restore: no identity")
	}
	args := []string{"restore", object, "--yes"}
	if unrecorded {
		args = append(args, "--unrecorded")
	}
	return args, map[string]string{"identity": identity}, nil
}

// BackupSettings runs the kept installer's `backup config --json --dir`: the settings as they are.
func (d Docker) BackupSettings(ctx context.Context) (*BackupSettings, error) {
	return d.BackupConfig(ctx, BackupConfig{})
}

// BackupConfig runs the kept installer's `backup config … --json --dir`, the rclone config a file.
func (d Docker) BackupConfig(ctx context.Context, c BackupConfig) (*BackupSettings, error) {
	args, files := backupConfigArgs(c)
	stdout, stderr, exit, err := d.stow(ctx, files, true, args...)
	if err != nil {
		return nil, fmt.Errorf("backup config: %w", err)
	}
	return backupOutcome(stdout, stderr, exit)
}

// Restore runs the kept installer's `restore OBJECT --yes [--unrecorded] --dir`, the identity a
// file: the backup loaded, the provision hook run on it.
func (d Docker) Restore(ctx context.Context, object, identity string, unrecorded bool) error {
	args, files, err := restoreArgs(object, identity, unrecorded)
	if err != nil {
		return err
	}
	_, stderr, exit, err := d.stow(ctx, files, true, args...)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return plainOutcome("restore "+object, stderr, exit)
}

// BackupSettings runs the VM's `sudo -n <short>-stow backup config --json`.
func (v VM) BackupSettings(ctx context.Context) (*BackupSettings, error) {
	return v.BackupConfig(ctx, BackupConfig{})
}

// BackupConfig runs the VM's `sudo -n <short>-stow backup config … --json`, the rclone config a
// file in a private directory on the VM.
func (v VM) BackupConfig(ctx context.Context, c BackupConfig) (*BackupSettings, error) {
	args, files := backupConfigArgs(c)
	stdout, stderr, exit, err := v.verb(ctx, files, args...)
	if err != nil {
		return nil, fmt.Errorf("backup config: %w", err)
	}
	return backupOutcome(stdout, stderr, exit)
}

// Restore runs the VM's `sudo -n <short>-stow restore OBJECT --yes [--unrecorded]`, the identity
// a file in a private directory on the VM: a job of its host daemon.
func (v VM) Restore(ctx context.Context, object, identity string, unrecorded bool) error {
	args, files, err := restoreArgs(object, identity, unrecorded)
	if err != nil {
		return err
	}
	_, stderr, exit, err := v.verb(ctx, files, args...)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return plainOutcome("restore "+object, stderr, exit)
}
