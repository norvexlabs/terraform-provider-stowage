package installer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// backupRunner answers every run with stdout, stderr and exit, recording its argv and stdin.
type backupRunner struct {
	argv   []string
	stdin  []byte
	stdout string
	stderr string
	exit   int
}

func (r *backupRunner) Run(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	r.argv, r.stdin = argv, stdin
	return []byte(r.stdout), []byte(r.stderr), r.exit, nil
}

const settingsJSON = `{"configured":true,"destination":"dav:backups","rclone_config_set":true,"rclone_config_from":"operator",` +
	`"recipients":["age1a","age1b"],"retention_days":30,"schedule":"daily","profile":"full","locked":false,"profiles":["full"]}`

const rcloneConf = "[dav]\ntype = webdav\npass = SECRET-PASS\n"

func TestBackupConfigArgs(t *testing.T) {
	t.Parallel()
	dest, days, sched, prof, lock := "dav:backups", 0, "daily", "full", false
	args, files := backupConfigArgs(BackupConfig{Destination: &dest, RcloneConfig: rcloneConf, Recipients: []string{"age1a", "age1b"},
		RetentionDays: &days, Schedule: &sched, Profile: &prof, Lock: &lock})
	want := []string{"backup", "config", "--destination", "dav:backups", "--recipient", "age1a", "--recipient", "age1b",
		"--retention-days", "0", "--schedule", "daily", "--profile", "full", "--unlock", "--json"}
	if !slices.Equal(args, want) {
		t.Errorf("args %q", args)
	}
	if files["rclone-config"] != rcloneConf || len(files) != 1 {
		t.Errorf("files %v", files)
	}
	// Nothing given: the settings read as they are, no file.
	args, files = backupConfigArgs(BackupConfig{})
	if !slices.Equal(args, []string{"backup", "config", "--json"}) || files != nil {
		t.Errorf("empty: %q %v", args, files)
	}
	keep, on := 2, true
	args, _ = backupConfigArgs(BackupConfig{KeepLocal: &keep, Lock: &on})
	if !slices.Equal(args, []string{"backup", "config", "--keep-local", "2", "--lock", "--json"}) {
		t.Errorf("keep_local: %q", args)
	}
}

func TestBackupConfigOnTargets(t *testing.T) {
	t.Parallel()
	dest := "dav:backups"
	for _, kind := range []string{"docker", "vm"} {
		r := &backupRunner{stdout: settingsJSON}
		var s *BackupSettings
		var err error
		c := BackupConfig{Destination: &dest, RcloneConfig: rcloneConf, Recipients: []string{"age1a"}}
		if kind == "docker" {
			s, err = Docker{Runner: r, WorkDir: "/w", Name: "notes", Short: "notes"}.BackupConfig(context.Background(), c)
		} else {
			s, err = VM{Runner: r, Short: "notes"}.BackupConfig(context.Background(), c)
		}
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !s.Configured || s.Destination != "dav:backups" || !s.RcloneConfigHeld() || !slices.Equal(s.Recipients, []string{"age1a", "age1b"}) || s.RetentionDays != 30 {
			t.Errorf("%s: settings %+v", kind, s)
		}
		// The rclone config's credentials ride standard input, never the command line.
		if strings.Contains(strings.Join(r.argv, " "), "SECRET-PASS") {
			t.Errorf("%s: the rclone config is on the command line: %q", kind, r.argv)
		}
		if got := untar(t, r.stdin)["rclone-config"]; got != rcloneConf {
			t.Errorf("%s: the rclone config sent is %q", kind, got)
		}
		if !slices.Contains(r.argv, "rclone-config") {
			t.Errorf("%s: the run does not name the rclone config's file: %q", kind, r.argv)
		}
	}
}

func TestBackupOutcome(t *testing.T) {
	t.Parallel()
	if s, err := backupOutcome([]byte(settingsJSON), nil, 0); err != nil || s.Profile != "full" {
		t.Errorf("done: %+v %v", s, err)
	}
	// An answer that does not read is never quoted.
	if _, err := backupOutcome([]byte(`{"configured":"SECRET"}`), nil, 0); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("a malformed answer: %v", err)
	}
	if _, err := backupOutcome([]byte(settingsJSON+"{}"), nil, 0); err == nil {
		t.Error("data after the answer was read")
	}
	// Each field every answer holds is required.
	for _, f := range backupSettingsFields {
		cut := strings.Replace(settingsJSON, `"`+f+`":`, `"other_`+f+`":`, 1)
		if _, err := backupOutcome([]byte(cut), nil, 0); err == nil || !strings.Contains(err.Error(), "holds no "+f) {
			t.Errorf("an answer without %s: %v", f, err)
		}
	}
	var refused *RefusedError
	if _, err := backupOutcome(nil, []byte("error: keep_local is the operator's\n"), 3); !errors.As(err, &refused) || !strings.Contains(refused.Refusal.Reason, "operator") {
		t.Errorf("exit 3: %v", err)
	}
	// An installer before --keep-local names the release that has it.
	var failed *FailedError
	if _, err := backupOutcome(nil, []byte("Error: unknown flag: --keep-local\n"), 1); !errors.As(err, &failed) || failed.Cause == nil || !strings.Contains(failed.Cause.Error(), KeepLocalMinStowage) {
		t.Errorf("unknown flag: %v", err)
	}
	if _, err := backupOutcome(nil, []byte("boom\n"), 1); !errors.As(err, &failed) || failed.Cause != nil {
		t.Errorf("exit 1: %v", err)
	}
}

func TestBackupSettingsRcloneHeld(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		s    BackupSettings
		held bool
	}{
		{BackupSettings{RcloneConfigSet: true, RcloneConfigFrom: "operator"}, true},
		{BackupSettings{RcloneConfigSet: true, RcloneConfigFrom: "unknown"}, false},
		{BackupSettings{RcloneConfigSet: false, RcloneConfigFrom: "operator"}, false},
	} {
		if c.s.RcloneConfigHeld() != c.held {
			t.Errorf("%+v: held %v", c.s, !c.held)
		}
	}
}

func TestRestoreOnTargets(t *testing.T) {
	t.Parallel()
	const key = "AGE-SECRET-KEY-1SECRETKEY"
	for _, kind := range []string{"docker", "vm"} {
		for _, unrecorded := range []bool{false, true} {
			r := &backupRunner{}
			var err error
			if kind == "docker" {
				err = Docker{Runner: r, WorkDir: "/w", Name: "notes", Short: "notes"}.Restore(context.Background(), "notes-full-x.pgdump.age", key, unrecorded)
			} else {
				err = VM{Runner: r, Short: "notes"}.Restore(context.Background(), "notes-full-x.pgdump.age", key, unrecorded)
			}
			if err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			argv := strings.Join(r.argv, " ")
			if !strings.Contains(argv, "restore notes-full-x.pgdump.age --yes") || strings.Contains(argv, "--unrecorded") != unrecorded {
				t.Errorf("%s: argv %q", kind, r.argv)
			}
			if strings.Contains(argv, "SECRETKEY") || untar(t, r.stdin)["identity"] != key {
				t.Errorf("%s: the identity is not on standard input alone: %q", kind, r.argv)
			}
		}
		r := &backupRunner{stderr: "error: notes-x is not a backup of notes\n", exit: 3}
		var refused *RefusedError
		if err := (VM{Runner: r, Short: "notes"}).Restore(context.Background(), "notes-x", key, false); !errors.As(err, &refused) {
			t.Errorf("exit 3: %v", err)
		}
	}
	if _, _, err := restoreArgs("o", "", false); err == nil {
		t.Error("a restore without an identity")
	}
	// An object that would read as a flag (as root, on the VM) or is no name is refused.
	for _, o := range []string{"", "--input=/var/lib/x", "-x", "a b", "a\nb"} {
		if _, _, err := restoreArgs(o, "AGE-SECRET-KEY-1", false); err == nil {
			t.Errorf("object %q was given", o)
		}
	}
}
