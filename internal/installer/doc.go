// Package installer reads a Stowage installer's machine interface: `describe --json`,
// `status --json`, the `--json` result of the verbs that change an install, and their exit
// status (0 done or nothing to do, 3 refused, any other failed).
//
// Decoding is strict where it decides and tolerant where the installer may grow: a field this
// package does not know is ignored (an installer adds fields without moving `format`), a known
// field of the wrong type, trailing data after the object and a missing required field are
// errors, and an unknown `format` is refused, naming what reads it.
package installer
