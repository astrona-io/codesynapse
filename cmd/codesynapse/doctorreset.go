package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
)

// runReset lists what the assistant setup left behind and, after a
// confirmation, removes it.
func (o *doctorOptions) runReset(ctx context.Context, home, cwd string) error {
	token, err := tokenFile()
	if err != nil {
		return err
	}
	actions, notes := agentcfg.PlanReset(agentcfg.ResetOptions{Home: home, Dir: cwd, TokenFile: token, KeepToken: o.keepToken})
	out := o.EC.Stderr
	if len(actions) == 0 {
		_, _ = fmt.Fprintln(out, "Nothing to reset: no CodeSynapse setup found for the assistants on this machine.")
	}
	for _, a := range actions {
		_, _ = fmt.Fprintln(out, "  • "+a.Description)
	}
	for _, n := range notes {
		_, _ = fmt.Fprintln(out, "  note: "+n)
	}
	if len(actions) == 0 {
		return nil
	}
	if !o.yes {
		_, _ = fmt.Fprint(out, "\nRemove these? Edited files are backed up as .bak. [y/N] ")
		line, _ := bufio.NewReader(o.EC.Stdin).ReadString('\n') //nolint:errcheck // EOF means no
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			return errors.New("cancelled; nothing was changed")
		}
	}
	failed := 0
	for _, a := range actions {
		if err := a.Apply(ctx); err != nil {
			failed++
			_, _ = fmt.Fprintf(out, "  ✗ %s: %v\n", a.Description, err)
			continue
		}
		_, _ = fmt.Fprintln(out, "  ✓ "+a.Description)
	}
	if failed > 0 {
		return fmt.Errorf("%d item(s) could not be removed; see the errors above", failed)
	}
	_, _ = fmt.Fprintln(out, "\nDone. Restart the assistants. Git hooks (`codesynapse git-hook uninstall`) and a cluster install (`codesynapse uninstall`) are separate.")
	return nil
}
