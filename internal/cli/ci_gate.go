package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cigate"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/generator"
)

// newCIGateCmd builds `forge ci gate`: the local CI gate. A scaffolded
// workflow is a thin wrapper that calls `task ci:checks`, `task test` and
// `task test:integration`; `task ci` runs all three on a laptop. The step list
// and the tool pins live here, in forge, so every project gets the same gate
// and a fix to it reaches every project on the next forge bump — instead of
// being a script each project copied once and owns forever. A project adds
// checks under forge.yaml `ci.gate.extra_checks`. See package cigate for the
// four rules every run obeys.
func newCIGateCmd() *cobra.Command {
	var (
		list   bool
		skip   []string
		groups bool
	)
	cmd := &cobra.Command{
		Use:   "gate [checks|test|integration|all]",
		Short: "Run the local CI gate: every check, one summary table, non-zero if any failed or was skipped",
		Long: "Runs the project's CI checks locally, exactly as the scaffolded workflow does.\n\n" +
			"Lanes (default: all):\n" +
			"  checks       verify-generated, lint, build, vuln-scan, license-check, KCL validation,\n" +
			"               deployability, and any ci.gate.extra_checks from forge.yaml   (`task ci:checks`)\n" +
			"  test         the unit suite, via `task test`\n" +
			"  integration  the integration suite, via `task test:integration`\n" +
			"  all          the three lanes, in that order                                  (`task ci`)\n\n" +
			"Four rules:\n" +
			"  1. Every check runs even if an earlier one failed; a summary table follows and the\n" +
			"     exit status is non-zero if any failed.\n" +
			"  2. A check that could not run (missing tool or prerequisite) is SKIPPED, not PASS,\n" +
			"     and fails the gate unless CI_SKIP names it: CI_SKIP=golangci-lint,license-check.\n" +
			"     The summary still lists it. N/A rows (nothing for the check to look at) are fine.\n" +
			"  3. Pinned tools (golangci-lint, buf, govulncheck, go-licenses) run at the versions the\n" +
			"     scaffolded workflow installs, from forge's tool cache, never from a different\n" +
			"     version that happens to be on PATH.\n" +
			"  4. Under GitHub Actions the table is appended to $GITHUB_STEP_SUMMARY.\n\n" +
			"Change detection (skip jobs a docs-only change cannot affect) is a CI cost optimization and\n" +
			"stays in the workflow. Locally you scope by choosing a lane.",
		Example: "  forge ci gate                       # the whole gate\n" +
			"  forge ci gate checks                # static + build lane only\n" +
			"  forge ci gate --list                # every check and its lane\n" +
			"  CI_SKIP=vuln-go forge ci gate       # offline: allow one skip, still listed",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			lanes, label, err := gateLanes(args)
			if err != nil {
				return err
			}
			root, err := projectRoot()
			if err != nil {
				return err
			}
			store, err := loadProjectStore()
			if err != nil {
				return err
			}
			cfg := store.Config()
			forge, err := forgeExecCommand()
			if err != nil {
				return fmt.Errorf("resolve forge binary: %w", err)
			}
			all, err := cigate.Plan(cigate.PlanInput{
				Root:  root,
				CI:    generator.CIWorkflowDataFor(root, cfg, generator.CIInputs{Frontends: ciFrontends(root, cfg)}),
				Cfg:   cfg,
				Forge: forge,
			})
			if err != nil {
				return cliutil.UserErr("forge ci gate", err.Error(), "forge.yaml (ci.gate)", "fix the extra check and re-run")
			}
			selected := cigate.Select(all, lanes)

			if list {
				for _, c := range selected {
					fmt.Fprintf(cmd.OutOrStdout(), "%-14s %s\n", c.Lane, c.Name)
				}
				return nil
			}

			skips := append(cigate.ParseSkips(os.Getenv(cigate.SkipEnv)), skip...)
			if bad := cigate.UnknownSkips(skips, all); len(bad) > 0 {
				names := make([]string, 0, len(all))
				for _, c := range all {
					names = append(names, c.Name)
				}
				return cliutil.UserErr("forge ci gate",
					fmt.Sprintf("%s names no check: %s — a misspelled skip would silently skip nothing", cigate.SkipEnv, strings.Join(bad, ", ")),
					"", "valid names: "+strings.Join(names, ", "))
			}

			onActions := os.Getenv("GITHUB_ACTIONS") == "true"
			rep := cigate.Run(cmd.Context(), label, selected, cigate.Options{
				Out:      cmd.OutOrStdout(),
				Skip:     skips,
				Root:     root,
				Groups:   groups || onActions,
				Resolver: cigate.NewResolver(root, cmd.ErrOrStderr()),
			})
			rep.RenderText(cmd.OutOrStdout())
			if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
				if err := appendStepSummary(path, rep); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not write the job summary: %v\n", err)
				}
			}
			if rep.Failed() {
				return gateFailure{}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "Print the checks of the selected lanes and exit")
	cmd.Flags().StringSliceVar(&skip, "skip", nil, "Check names to skip, in addition to CI_SKIP (still listed, as SKIPPED)")
	cmd.Flags().BoolVar(&groups, "groups", false, "Wrap each check's output in GitHub Actions ::group:: lines (automatic under GITHUB_ACTIONS)")
	return cmd
}

// gateFailure is the gate's verdict as an error. The summary table above
// already said everything, so its message adds nothing and main prints one
// short line.
type gateFailure struct{}

func (gateFailure) Error() string { return "the CI gate failed (see the summary above)" }

func gateLanes(args []string) ([]cigate.Lane, string, error) {
	which := "all"
	if len(args) == 1 {
		which = args[0]
	}
	switch which {
	case "all":
		return []cigate.Lane{cigate.LaneChecks, cigate.LaneTest, cigate.LaneIntegration}, "all", nil
	case "checks":
		return []cigate.Lane{cigate.LaneChecks}, "checks", nil
	case "test":
		return []cigate.Lane{cigate.LaneTest}, "test", nil
	case "integration":
		return []cigate.Lane{cigate.LaneIntegration}, "integration", nil
	}
	return nil, "", cliutil.UserErr("forge ci gate", fmt.Sprintf("unknown lane %q", which), "", "use one of: checks, test, integration, all")
}

func appendStepSummary(path string, rep cigate.Report) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	rep.RenderMarkdown(f)
	return nil
}
