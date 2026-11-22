package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
	"github.com/HunterLzap/slopguard/internal/core"
	"github.com/HunterLzap/slopguard/internal/reviewer"
)

const usage = "Usage: slopguard init|verify|status|proposals|approve|dismiss|mcp|hook|agent"

func RunCLI(ctx context.Context, args []string, in io.Reader, out, log io.Writer, runner core.ProcessRunner, client reviewer.HTTPClient) (exit int) {
	if len(args) == 0 {
		fmt.Fprintln(log, usage)
		return 2
	}
	command, args := args[0], args[1:]
	if command == "help" || command == "--help" || command == "-h" {
		fmt.Fprintln(out, usage)
		return 0
	}
	printError := func(err error) int {
		fmt.Fprintln(log, err)
		return 2
	}
	if command == "agent" {
		if err := runAgent(args, out); err != nil {
			return printError(err)
		}
		return 0
	}
	var path, scope, id, mode, operation string
	var jsonOutput bool
	flags := flag.NewFlagSet("slopguard "+command, flag.ContinueOnError)
	flags.SetOutput(log)
	if command != "mcp" {
		flags.StringVar(&path, "root", ".", "project directory")
	}
	switch command {
	case "verify":
		flags.StringVar(&scope, "scope", "changed", "changed or all")
		flags.BoolVar(&jsonOutput, "json", false, "write JSON")
	case "status", "proposals":
		flags.BoolVar(&jsonOutput, "json", false, "write JSON")
	case "approve":
		flags.StringVar(&id, "id", "", "proposal ID")
	case "dismiss":
		flags.StringVar(&id, "id", "", "proposal ID")
		flags.StringVar(&mode, "mode", "dismiss", "dismiss or snooze")
	case "init", "mcp":
	case "hook":
		if len(args) == 0 || args[0] != "install" && args[0] != "uninstall" {
			return printError(fmt.Errorf("usage: slopguard hook install|uninstall [--root directory]"))
		}
		operation, args = args[0], args[1:]
	default:
		return printError(fmt.Errorf("unknown command %q\n%s", command, usage))
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return printError(fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " ")))
	}
	if command == "verify" && scope != "all" && scope != "changed" {
		return printError(fmt.Errorf("invalid scope %q; use changed or all", scope))
	}
	if (command == "approve" || command == "dismiss") && strings.TrimSpace(id) == "" {
		return printError(fmt.Errorf("--id is required"))
	}
	store, err := reviewer.OpenDefault(ctx)
	if err != nil {
		return printError(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			fmt.Fprintln(log, err)
			if command != "verify" && exit == 0 {
				exit = 2
			}
		}
	}()
	app := NewService(store, runner, client)
	switch command {
	case "mcp":
		err = app.ServeMCP(ctx, in, out, log)
	case "init":
		var result core.InitResult
		result, err = app.Init(ctx, path)
		if err == nil {
			if result.AlreadyExists {
				_, err = fmt.Fprintf(out, "Kept existing project config %s\n", result.Path)
			} else {
				_, err = fmt.Fprintf(out, "Created project config %s\n", result.Path)
			}
		}
	case "verify":
		result, verifyErr := app.Verify(ctx, path, contract.VerifyScope(scope), true)
		if verifyErr != nil {
			return printError(verifyErr)
		}
		if jsonOutput {
			err = writeJSON(out, result)
		} else {
			err = writeVerify(out, result)
		}
		if err != nil {
			fmt.Fprintln(log, err)
		}
		return result.ExitCode
	case "status":
		var status Status
		status, err = app.Status(ctx, path)
		if err == nil {
			if jsonOutput {
				err = writeJSON(out, status)
			} else {
				last := string(status.LastStatus)
				if last == "" {
					last = "not yet scanned"
				}
				_, err = fmt.Fprintf(out, "%s\nLanguages: %s\nLast scan: %s\nEnabled rules: %d\nPending proposals: %d\nHook enabled: %t\n",
					status.Path, strings.Join(status.Languages, ", "), last, status.AppliedRules, status.PendingProposals, status.HookEnabled)
			}
		}
	case "proposals":
		var proposals []contract.RuleProposal
		proposals, err = app.Proposals(ctx, path)
		if err == nil {
			if jsonOutput {
				err = writeJSON(out, proposals)
			} else {
				err = writeProposals(out, proposals)
			}
		}
	case "approve", "dismiss":
		var proposal contract.RuleProposal
		if command == "approve" {
			proposal, err = app.Approve(ctx, path, id)
		} else {
			proposal, err = app.Dismiss(ctx, path, id, mode)
		}
		if err == nil {
			_, err = fmt.Fprintf(out, "%s %s\n", proposal.Status, proposal.ID)
		}
	case "hook":
		err = app.SetHook(ctx, path, operation == "install")
		if err == nil {
			_, err = fmt.Fprintf(out, "SlopGuard pre-commit hook %sed\n", operation)
		}
	}
	if err != nil {
		return printError(err)
	}
	return 0
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeVerify(out io.Writer, result contract.VerifyResult) error {
	var text strings.Builder
	fmt.Fprintf(&text, "%s (%s, %d files)\n", result.Status, result.Scope, result.ScannedFilesCount)
	if len(result.ExecutedCheckers) > 0 {
		fmt.Fprintln(&text, "Checkers:", strings.Join(result.ExecutedCheckers, ", "))
	}
	for _, tool := range result.MissingTools {
		fmt.Fprintf(&text, "Missing %s: %s\n", tool.Name, tool.InstallCommand)
	}
	for _, diagnostic := range result.Diagnostics {
		where := diagnostic.File
		if diagnostic.Line > 0 {
			where += fmt.Sprintf(":%d:%d", diagnostic.Line, diagnostic.Column)
		}
		fmt.Fprintf(&text, "%s %s %s: %s\n", where, diagnostic.Checker, diagnostic.Severity, diagnostic.Message)
	}
	for _, warning := range result.Warnings {
		fmt.Fprintln(&text, "Warning:", warning)
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func writeProposals(out io.Writer, proposals []contract.RuleProposal) error {
	var text strings.Builder
	if len(proposals) == 0 {
		fmt.Fprintln(&text, "No pending proposals")
	}
	for _, p := range proposals {
		fmt.Fprintf(&text, "%s %s %s", p.ID, p.Decision, p.SummaryZH)
		if p.Decision == contract.HardRule {
			fmt.Fprintf(&text, " (sandbox sample hits: %d, project hits: %d)", p.SandboxPositiveHits, p.SandboxProjectHits)
		}
		fmt.Fprintln(&text)
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func runAgent(args []string, out io.Writer) error {
	if len(args) == 0 || len(args) > 2 {
		return fmt.Errorf("usage: slopguard agent list|enable|disable|snippet [agent_id]")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if args[0] == "snippet" {
		if len(args) == 1 {
			snippets, err := ExportGenericMCPConfig(executable)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(out, "%s\n%s", snippets.JSON, snippets.TOML)
			return err
		}
		for _, a := range agentDefinitions {
			if a.id == args[1] {
				if a.format == dshConfig {
					_, err = io.WriteString(out, dshRow(executable))
					return err
				}
				data, err := a.patch(nil, executable)
				if err != nil {
					return err
				}
				_, err = out.Write(data)
				return err
			}
		}
		return fmt.Errorf("unknown agent %q", args[1])
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir, err := core.GlobalConfigDir()
	if err != nil {
		return err
	}
	agent := AgentSwitch{Home: home, ConfigDir: dir, Executable: executable}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("agent list takes no arguments")
		}
		entries, err := agent.List()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			state := "disabled"
			if entry.Enabled {
				state = "enabled"
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", entry.ID, state, entry.ConfigPath); err != nil {
				return err
			}
		}
		return nil
	case "enable", "disable":
		if len(args) != 2 {
			return fmt.Errorf("agent ID is required")
		}
		if err := agent.Set(args[1], args[0] == "enable"); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%sd SlopGuard for %s\n", args[0], args[1])
		return err
	default:
		return fmt.Errorf("unknown agent action %q", args[0])
	}
}
