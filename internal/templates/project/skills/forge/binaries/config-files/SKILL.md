---
name: config-files
description: Loading a binary's typed config from a YAML/JSON file — --config and the per-binary <BINARY>_CONFIG path variable, precedence, the override, and what a missing or invalid file does.
---

# Config files

Every config also loads from a file: `--config <path>` (`.yaml`, `.yml` or `.json`, keyed by the proto field names), or — when the flag is absent — the binary's OWN path variable, `<BINARY>_CONFIG`: `RELIANT_CONFIG` for `cmd/reliant`, `IDP_PROVISION_ONESHOT_CONFIG` for `cmd/idp-provision-oneshot`. A binary's root config and its command configs share it. The project-global `AppConfig`, bound to no binary, reads `<PROJECT>_CONFIG` (`config.ConfigPathEnv` in the generated `pkg/config`). Precedence is defaults < file < env < flags.

- The variable is per binary on purpose: one shared name would let a shell that sets it for one app make every other forge binary launched from that shell load a file written for a different schema and fail at startup.
- A path that is given but missing, unreadable or invalid fails startup, naming where it came from (`config file "/x.yaml" (from env RELIANT_CONFIG) does not exist`). No path given is not an error: the file layer is skipped. A variable set to `""` is unset, like every variable the loader reads.
- To answer to a different variable, call `forgeconfig.RegisterConfigFlag(root, "MY_APP_CONFIG")` (`forgeconfig` = `github.com/reliant-labs/forge/pkg/config`) on the binary's root command BEFORE `config.Register<Msg>Flags(root)`: the first `--config` registered on a command path names the variable for every load beneath it.
