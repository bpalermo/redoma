// Package msg is the lead's client of the relay's local API: `redoma msg`
// tasks agents, answers their questions, cancels their tasks and watches
// what they send, and `redoma mcp` serves the same as MCP tools on stdio.
//
//	redoma msg send <agent> <text>...    queue a task; prints its id
//	redoma msg reply <question> <text>... answer a question (<agent>/<id>)
//	redoma msg cancel <task>             cancel a task (<agent>/<id>)
//	redoma msg watch [<agent>]           print what agents send, one JSON object per line
//	redoma mcp                           serve send, reply, cancel and poll as MCP tools on stdio
//
// A text of "-" is read from the standard input.
package msg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	relayv1 "github.com/bpalermo/redoma/api/relay/v1"
	"github.com/bpalermo/redoma/common/version"
	"github.com/bpalermo/redoma/msg/internal/feed"
	"github.com/bpalermo/redoma/msg/internal/leadmcp"
	"github.com/bpalermo/redoma/msg/internal/lines"
)

// DefaultSocket is the relay's lead socket.
const DefaultSocket = "/run/redoma-relay-lead/lead.sock"

// subcommands are `redoma msg`'s, with their usage, in the order help
// lists them.
var subcommands = []struct{ use, short string }{
	{"send <agent> <text>...", "Queue a task for the agent; prints the task id"},
	{"reply <question> <text>...", "Answer a question (its id, <agent>/<id>, as watch prints it)"},
	{"cancel <task>", "Cancel a task, running or queued"},
	{"watch [<agent>]", "Print what the agents (or one) send, one JSON object per line"},
}

// Command is `redoma msg`.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "msg",
		Short: "Task agents, answer their questions, cancel tasks and watch what they send",
		Long:  "The lead's client of the relay's lead socket. A text of \"-\" is read from the standard input.",
	}
	socket := cmd.PersistentFlags().String("socket", DefaultSocket, "the relay's lead socket")
	for _, sub := range subcommands {
		name, _, _ := strings.Cut(sub.use, " ")
		cmd.AddCommand(&cobra.Command{
			Use:   sub.use,
			Short: sub.short,
			RunE: func(cmd *cobra.Command, args []string) error {
				return withClient(*socket, func(c relayv1.LeadClient) error {
					err := commands[name](cmd.Context(), c, args, stdio{in: cmd.InOrStdin(), out: cmd.OutOrStdout()})
					if errors.Is(err, errUsage) {
						return fmt.Errorf("usage: redoma msg %s", sub.use)
					}
					return err
				})
			},
		})
	}
	return cmd
}

// MCPCommand is `redoma mcp`.
func MCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve send, reply, cancel and poll as MCP tools on stdio, for a lead that is a Claude Code session",
		Args:  cobra.NoArgs,
	}
	socket := cmd.Flags().String("socket", DefaultSocket, "the relay's lead socket")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return withClient(*socket, func(c relayv1.LeadClient) error {
			return serveMCP(cmd.Context(), c, feed.DefaultConfig, stdio{in: cmd.InOrStdin(), out: cmd.OutOrStdout()})
		})
	}
	return cmd
}

// withClient runs fn with a client of the lead socket.
func withClient(socket string, fn func(relayv1.LeadClient) error) error {
	conn, err := grpc.NewClient("unix:"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(relayv1.NewLeadClient(conn))
}

var errUsage = errors.New("bad usage")

// stdio is a command's standard input and output.
type stdio struct {
	in  io.Reader
	out io.Writer
}

// command runs one subcommand with its arguments.
type command func(ctx context.Context, c relayv1.LeadClient, args []string, std stdio) error

var commands = map[string]command{
	"send": func(ctx context.Context, c relayv1.LeadClient, args []string, std stdio) error {
		if len(args) < 2 {
			return errUsage
		}
		text, err := textOf(args[1:], std.in)
		if err != nil {
			return err
		}
		r, err := c.Send(ctx, &relayv1.SendRequest{Agent: args[0], Text: text})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(std.out, r.GetTaskId())
		return err
	},
	"reply": func(ctx context.Context, c relayv1.LeadClient, args []string, std stdio) error {
		if len(args) < 2 {
			return errUsage
		}
		text, err := textOf(args[1:], std.in)
		if err != nil {
			return err
		}
		_, err = c.Reply(ctx, &relayv1.ReplyRequest{QuestionId: args[0], Text: text})
		return err
	},
	"cancel": func(ctx context.Context, c relayv1.LeadClient, args []string, _ stdio) error {
		if len(args) != 1 {
			return errUsage
		}
		_, err := c.Cancel(ctx, &relayv1.CancelRequest{TaskId: args[0]})
		return err
	},
	"watch": func(ctx context.Context, c relayv1.LeadClient, args []string, std stdio) error {
		if len(args) > 1 {
			return errUsage
		}
		return watch(ctx, c, strings.Join(args, ""), std.out)
	},
}

// serveMCP serves the MCP session on std until its input ends, with one
// Watch, for poll, until then.
func serveMCP(ctx context.Context, c relayv1.LeadClient, cfg feed.Config, std stdio) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f := feed.New(c, cfg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	err := leadmcp.New(c, f, version.Version).MCP().Serve(ctx, std.in, std.out)
	cancel()
	<-done
	return err
}

func run(ctx context.Context, c relayv1.LeadClient, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return errUsage
	}
	return cmd(ctx, c, args[1:], stdio{in: stdin, out: stdout})
}

// textOf is the words of a text, or the standard input for "-".
func textOf(words []string, stdin io.Reader) (string, error) {
	if len(words) == 1 && words[0] == "-" {
		b, err := io.ReadAll(stdin)
		return string(b), err
	}
	return strings.Join(words, " "), nil
}

func watch(ctx context.Context, c relayv1.LeadClient, agent string, stdout io.Writer) error {
	st, err := c.Watch(ctx, &relayv1.WatchRequest{Agent: agent})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	for {
		ev, err := st.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := enc.Encode(lines.Raw(ev)); err != nil {
			return err
		}
	}
}
