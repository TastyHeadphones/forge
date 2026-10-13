package config

// First-class config-FILE loading layer.
//
// This file adds a config file as a real, always-wired precedence layer to the
// descriptor-driven loader. The full precedence is:
//
//	defaults  →  config file  →  environment  →  flags
//	(earliest)                                   (wins)
//
// Each later layer OVERRIDES the earlier ones. The file layer is NOT optional
// and NOT dormant: RegisterFlags always registers a persistent `--config`
// flag, and Load always consults it (plus its environment twin).
//
// Resolution of the file path (see resolveConfigPath):
//
//   - the `--config <path>` flag, when explicitly set on this invocation, OR
//   - the binary's own path variable, <BINARY>_CONFIG (RELIANT_CONFIG for the
//     reliant binary; see ConfigPathEnvFor and configPathEnv).
//
// The flag wins over the variable. If neither is given, NO file is loaded and
// the loader proceeds with defaults → env → flags. That is normal precedence,
// not a fallback.
//
// The variable is named after the BINARY on purpose. A name every forge-built
// binary shared (it was FORGE_CONFIG) meant a shell that set it for one app
// made every other forge binary started from that shell — a desktop app's
// daemon inheriting the login environment, say — load a file written for a
// different schema and die at startup. A config file is only meaningful to the
// binary whose config messages it was written against, so its variable is that
// binary's alone.
//
// LOUD-ON-MISSING (no silent fallback): when a config path IS explicitly given
// (flag or env) but the file is missing, unreadable, or invalid, Load returns
// an error and aborts, naming where the path came from. A bad explicit path is
// never swallowed.
//
// Formats: protojson for `.json`; for `.yaml`/`.yml` the bytes are converted
// YAML→JSON (sigs.k8s.io/yaml) and then unmarshaled with protojson. Unmarshal
// is proto-native and generic over ANY config message: it honors the proto's
// own JSON field names and descends into nested config blocks automatically.
//
// Sensitive fields: the file MAY set a sensitive field (it is a file on disk,
// not shell history), exactly as env may. Sensitive fields still never get a
// flag. The protojson unmarshal does not know about the (sensitive) annotation
// — it simply sets whatever the file names — which is the intended behavior:
// file/env yes, flag no.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/reliant-labs/forge/pkg/forgepb"
)

// ConfigFlag is the name of the always-registered persistent flag that points
// the loader at a config file. RegisterFlags registers it unconditionally.
const ConfigFlag = "config"

// configPathEnvAnnotation is the pflag annotation on the --config flag that
// carries the name of its environment twin. Recording the variable ON the flag
// makes the two one declaration, the way a field's flag and env_var are: every
// command that inherits --config resolves the same variable, whichever config
// message it is loading.
const configPathEnvAnnotation = "forge.config/path-env"

// ConfigPathEnvFor returns the environment variable that names binary's config
// file when --config is not given: the binary name upper-cased with every
// character that cannot appear in a variable name replaced by "_", plus
// "_CONFIG". "reliant" → RELIANT_CONFIG, "idp-provision-oneshot" →
// IDP_PROVISION_ONESHOT_CONFIG. "" for an empty binary.
//
// It is the single derivation both halves use: the runtime, for a message
// carrying (forge.v1.binary_config).binary, and forge's generator, for the
// generated help and docs and for a project-global config (named after the
// project).
func ConfigPathEnvFor(binary string) string {
	if binary == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(binary) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String() + "_CONFIG"
}

// RegisterConfigFlag registers the persistent --config <path> flag on cmd,
// with pathEnv as its environment twin: the variable that supplies the path
// when the flag is not given. An empty pathEnv registers a flag with no twin.
//
// It is a no-op when cmd or a parent already carries --config — the first
// registration on a command path names the variable for that whole path. That
// is what makes it an override: call it on a binary's root command before the
// generated Register*Flags, and every load beneath that root reads the
// variable named here instead of the derived <BINARY>_CONFIG.
//
// The generated pkg/config calls it for a project-global config, which carries
// no (forge.v1.binary_config) annotation to derive a name from.
func RegisterConfigFlag(cmd *cobra.Command, pathEnv string) {
	if lookupConfigFlag(cmd) != nil {
		return // own or inherited: a second declaration would shadow it
	}
	usage := "path to a config file (.json/.yaml/.yml). Precedence: defaults < file < env < flags"
	if pathEnv != "" {
		usage = fmt.Sprintf("path to a config file (.json/.yaml/.yml); also settable via %s. "+
			"Precedence: defaults < file < env < flags", pathEnv)
	}
	pf := cmd.PersistentFlags()
	pf.String(ConfigFlag, "", usage)
	if pathEnv != "" {
		// SetAnnotation only fails for a flag that does not exist; it was
		// registered on the line above.
		_ = pf.SetAnnotation(ConfigFlag, configPathEnvAnnotation, []string{pathEnv})
	}
}

// binaryOf returns the binary msg is bound to by its
// (forge.v1.binary_config) annotation, or "" for an unbound message.
func binaryOf(msg proto.Message) string {
	opts := msg.ProtoReflect().Descriptor().Options()
	if opts == nil {
		return ""
	}
	bc, ok := proto.GetExtension(opts, forgepb.E_BinaryConfig).(*forgepb.BinaryConfigOptions)
	if !ok || bc == nil {
		return ""
	}
	return bc.GetBinary()
}

// lookupConfigFlag finds the --config flag cmd resolves: its own, or one it
// inherits from a parent's persistent flags. cobra merges inherited
// persistent flags into Flags() only when a command executes, so the parents
// are walked as well — a load from a command that has not run yet (a test, a
// hand-built tree) must find the same flag the executed command would.
func lookupConfigFlag(cmd *cobra.Command) *pflag.Flag {
	if f := cmd.Flags().Lookup(ConfigFlag); f != nil {
		return f
	}
	for c := cmd; c != nil; c = c.Parent() {
		if f := c.PersistentFlags().Lookup(ConfigFlag); f != nil {
			return f
		}
	}
	return nil
}

// configPathEnv returns the variable that names msg's config file for a load
// from cmd: the twin recorded on the --config flag cmd resolves (see
// RegisterConfigFlag), else <BINARY>_CONFIG for a message bound to a binary,
// else "" — an unbound message loaded where no --config flag names a variable
// has no environment path, only --config.
func configPathEnv(cmd *cobra.Command, msg proto.Message) string {
	if cmd != nil {
		if f := lookupConfigFlag(cmd); f != nil {
			if v := f.Annotations[configPathEnvAnnotation]; len(v) > 0 && v[0] != "" {
				return v[0]
			}
		}
	}
	return ConfigPathEnvFor(binaryOf(msg))
}

// resolveConfigPath returns the explicit config-file path for this invocation,
// where it came from ("--config" or "env RELIANT_CONFIG", for error messages),
// and whether one was given. The --config flag (when Changed) wins over the
// path variable. A variable set to "" is unset, as every variable the loader
// reads is (lookupEnv). When this returns ok=false, no file is loaded (normal
// defaults→env→flags precedence).
func resolveConfigPath(cmd *cobra.Command, msg proto.Message) (path, source string, ok bool) {
	if cmd != nil {
		if f := lookupConfigFlag(cmd); f != nil && f.Changed && f.Value.String() != "" {
			return f.Value.String(), "--" + ConfigFlag, true
		}
	}
	if env := configPathEnv(cmd, msg); env != "" {
		if v, set := lookupEnv(env); set {
			return v, "env " + env, true
		}
	}
	return "", "", false
}

// applyConfigFile reads the file at path and OVERLAYS it onto msg, preserving
// the defaults layer for fields the file omits. It is the file precedence
// layer (between defaults and env). source names where the path came from
// ("--config", "env RELIANT_CONFIG") so a failure says what to fix.
//
// Implementation note: protojson.Unmarshal RESETS its target before decoding,
// so it cannot decode directly into the defaults-populated msg without wiping
// the defaults. Instead the file is decoded into a FRESH message of the same
// type and then proto.Merge'd onto msg: Merge copies the file's populated
// fields over the defaults and recurses into nested blocks, while fields the
// file did not populate keep their default. (Proto3 scalar presence is
// value-based: a file that explicitly writes a scalar's zero value cannot be
// distinguished from omitting it — declare such a field with a non-zero
// default or as optional if that distinction matters.)
//
// Any failure — unreadable file, unknown extension, malformed JSON/YAML, or a
// document that does not match the config schema — is returned as a loud error.
// Callers only invoke this when a path was EXPLICITLY given, so an error here
// always aborts Load (no silent fallback).
func applyConfigFile(msg proto.Message, path, source string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("config file %q (from %s) does not exist: %s", path, source, missingFileFix(source))
		}
		return fmt.Errorf("config file %q (from %s): %w", path, source, err)
	}

	jsonBytes, err := toJSON(path, raw)
	if err != nil {
		return fmt.Errorf("config file %q (from %s): %w", path, source, err)
	}

	// Decode into a fresh message of the same type (protojson resets it),
	// then merge the populated fields onto msg. DiscardUnknown=false makes a
	// stray/typo'd key a loud error rather than a silent drop.
	fileMsg := msg.ProtoReflect().New().Interface()
	opts := protojson.UnmarshalOptions{DiscardUnknown: false}
	if err := opts.Unmarshal(jsonBytes, fileMsg); err != nil {
		return fmt.Errorf("config file %q (from %s): invalid config document: %w", path, source, err)
	}
	proto.Merge(msg, fileMsg)
	return nil
}

// missingFileFix says how to recover from a missing config file, for the
// source the path came from. A path from the environment is the case worth
// spelling out: the operator may not know the variable is set at all.
func missingFileFix(source string) string {
	if env, ok := strings.CutPrefix(source, "env "); ok {
		return fmt.Sprintf("point %s at a readable file, or unset it", env)
	}
	return fmt.Sprintf("pass --%s a readable file, or omit it", ConfigFlag)
}

// toJSON returns protojson-ready JSON bytes for a config file, dispatching on
// the file extension: .json passes through; .yaml/.yml are converted via
// sigs.k8s.io/yaml (which round-trips through the YAML and JSON codecs). An
// unrecognized extension is a loud error — the format must be explicit.
func toJSON(path string, raw []byte) ([]byte, error) {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		return raw, nil
	case ".yaml", ".yml":
		j, err := sigsyaml.YAMLToJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("converting YAML to JSON: %w", err)
		}
		return j, nil
	default:
		return nil, fmt.Errorf("unsupported config file extension %q (want .json, .yaml, or .yml)", ext)
	}
}
