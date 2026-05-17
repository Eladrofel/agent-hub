// Package commands wires the agentctl subcommands. Each subcommand file
// follows the same template:
//
//  1. Define flags
//  2. In RunE: load config, build client, marshal request, run via runCall
//  3. runCall handles audit + best-effort posture + stdout/stderr output
//
// The central design decision lives in runCall: on any error, default
// behaviour is to log + audit + exit 0. The --strict flag (wired by Register)
// overrides this to exit 1. See the design plan §"Fail-closed semantics".
package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/audit"
	"github.com/Eladrofel/agent-hub/gateway/internal/agentctl/config"
)

// IO carries the std streams + an optional cwd, so tests can swap them.
type IO struct {
	Stdout io.Writer
	Stderr io.Writer
}

// cmdIO returns the IO bound to a cobra command. Tests use SetOut/SetErr
// to capture; production binaries fall back to os.Stdout/Stderr because
// cobra sets those by default. We do an explicit nil check because some
// tests build a bare cobra command without wiring streams.
func cmdIO(cmd *cobra.Command) IO {
	out := cmd.OutOrStdout()
	err := cmd.ErrOrStderr()
	return IO{Stdout: out, Stderr: err}
}

// strictFlag returns the persistent --strict flag value for the given cmd.
// Lookup walks up the parent chain — cobra propagates persistent flags down,
// but they're registered on root, so we read from there.
func strictFlag(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	v, _ := cmd.Flags().GetBool("strict")
	if v {
		return true
	}
	// Persistent flags on root sometimes live on the inherited set.
	v, _ = cmd.InheritedFlags().GetBool("strict")
	return v
}

// prettyFlag returns the value of the local --pretty flag (read commands).
func prettyFlag(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("pretty")
	return v
}

// jsonFlag returns the value of the local --json flag (mutating commands).
func jsonFlag(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("json")
	return v
}

// addStrictFlag registers --strict on the root command as a persistent flag.
func addStrictFlag(root *cobra.Command) {
	root.PersistentFlags().Bool("strict", false,
		"exit 1 on any error (default: best-effort, exit 0 with stderr log)")
}

// callOpts controls how runCall renders output and audits the call.
type callOpts struct {
	cmdName string
	args    any   // sanitised arg snapshot for the audit entry
	io      IO
	strict  bool
	auditor *audit.Writer

	// renderRead: if non-nil, called on the raw response body for read
	// commands. The default emits JSON on stdout.
	renderRead func(body []byte, pretty bool, w io.Writer) error
	pretty     bool

	// renderMutate: if non-nil, called for mutating commands to emit a
	// one-line success summary to stderr. If --json is set, the raw body is
	// written to stdout instead.
	renderMutate func(body []byte) (summary string, err error)
	emitJSON     bool
}

// runCall executes one call and applies the best-effort vs. strict posture.
// Always returns nil on best-effort failures; returns the error on --strict.
func runCall(
	ctx context.Context,
	opts callOpts,
	do func(ctx context.Context) (int, []byte, error),
) error {
	status, body, err := do(ctx)

	entry := audit.Entry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Command:    opts.cmdName,
		Args:       opts.args,
		HTTPStatus: status,
		Strict:     opts.strict,
	}

	if err != nil {
		entry.Outcome = "error"
		entry.Error = err.Error()
		opts.auditor.Append(entry)

		if opts.strict {
			fmt.Fprintf(opts.io.Stderr, "agentctl %s: %v; halting (--strict)\n", opts.cmdName, err)
			return err
		}
		fmt.Fprintf(opts.io.Stderr, "agentctl %s: %v; continuing (best-effort)\n", opts.cmdName, err)
		return nil
	}

	entry.Outcome = "ok"
	opts.auditor.Append(entry)

	// Success — render.
	switch {
	case opts.renderRead != nil:
		if err := opts.renderRead(body, opts.pretty, opts.io.Stdout); err != nil {
			fmt.Fprintf(opts.io.Stderr, "agentctl %s: render: %v\n", opts.cmdName, err)
			if opts.strict {
				return err
			}
		}
	case opts.renderMutate != nil:
		if opts.emitJSON {
			if _, err := opts.io.Stdout.Write(body); err != nil {
				return err
			}
			// Ensure trailing newline for shell-friendliness.
			_, _ = opts.io.Stdout.Write([]byte("\n"))
			return nil
		}
		summary, rerr := opts.renderMutate(body)
		if rerr != nil {
			fmt.Fprintf(opts.io.Stderr, "agentctl %s: render: %v\n", opts.cmdName, rerr)
			if opts.strict {
				return rerr
			}
		}
		if summary != "" {
			fmt.Fprintln(opts.io.Stderr, summary)
		}
	}
	return nil
}

// renderJSONResponse is the default renderRead — copies the body to stdout,
// indenting if pretty=true.
func renderJSONResponse(body []byte, pretty bool, w io.Writer) error {
	if !pretty {
		// Validate it parses; if not, write raw so we don't swallow gateway
		// quirks.
		var probe any
		if err := json.Unmarshal(body, &probe); err == nil {
			out, _ := json.Marshal(probe)
			_, err := w.Write(append(out, '\n'))
			return err
		}
		_, err := w.Write(body)
		return err
	}
	var probe any
	if err := json.Unmarshal(body, &probe); err != nil {
		_, werr := w.Write(body)
		return werr
	}
	out, err := json.MarshalIndent(probe, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

// loadAuthedConfig is a tiny wrapper that turns config errors into stderr
// messages + (when --strict) propagation. Read commands that require auth
// share this entry point.
func loadAuthedConfig(cmd *cobra.Command, requireAuth bool) (*config.Config, error) {
	cfg, err := config.Load(requireAuth)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "agentctl %s: %v\n", cmd.Name(), err)
		if strictFlag(cmd) {
			return nil, err
		}
		return nil, errSilent
	}
	return cfg, nil
}

// errSilent is returned when a best-effort command has already reported its
// failure and wants the cobra runner to exit 0. main.go checks for it.
var errSilent = errors.New("silent")

// IsSilent returns true if err is the sentinel best-effort exit-0 marker.
func IsSilent(err error) bool {
	return errors.Is(err, errSilent)
}
