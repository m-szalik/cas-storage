// Command cascli is a command-line client for cas-storage.
//
// The server and the API key come from the environment: CAS_URL (default
// http://localhost:8080) and CAS_API_KEY (required). The key is never a flag,
// so it stays out of the shell history and the process list.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/m-szalik/goutils"
	"github.com/spf13/cobra"

	"github.com/m-szalik/cas-storage/client"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

// errAbsent makes `exists` exit with 1 without printing an error.
var errAbsent = errors.New("object absent")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	baseURL := goutils.Env("CAS_URL", "http://localhost:8080")
	apiKey := goutils.Env("CAS_API_KEY", "")
	err := newRootCmd(baseURL, apiKey, os.Stdin, os.Stdout).ExecuteContext(ctx)
	switch {
	case err == nil:
	case errors.Is(err, errAbsent):
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "cascli:", err)
		os.Exit(2)
	}
}

func newRootCmd(baseURL, apiKey string, in io.Reader, out io.Writer) *cobra.Command {
	cl := func() *client.Client { return client.New(baseURL, client.WithAPIKey(apiKey)) }

	root := &cobra.Command{
		Use:     "cascli",
		Short:   "Command-line client for cas-storage",
		Version: version,
		Long: "Command-line client for cas-storage.\n\n" +
			"Environment:\n" +
			"  CAS_URL      server URL (default http://localhost:8080)\n" +
			"  CAS_API_KEY  API key (required)",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if apiKey == "" {
				return errors.New("CAS_API_KEY is not set")
			}
			return nil
		},
	}
	root.SetIn(in)
	root.SetOut(out)

	var putLabels []string
	put := &cobra.Command{
		Use:   "put <file|->",
		Short: "Store a file (- for stdin) and print its hash",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sel, err := parseLabels(putLabels, false)
			if err != nil {
				return err
			}
			r := cmd.InOrStdin()
			if args[0] != "-" {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer goutils.CloseQuietly(f)
				r = f
			}
			c := cl()
			hash, err := c.PutStream(cmd.Context(), r)
			if err != nil {
				return err
			}
			for _, k := range sortedKeys(sel) {
				if err := c.SetLabel(cmd.Context(), hash, k, sel[k]); err != nil {
					return fmt.Errorf("label %s: %w", k, err)
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), hash)
			return err
		},
	}
	put.Flags().StringArrayVarP(&putLabels, "label", "l", nil, "set a label key=value on the object (repeatable)")

	var output string
	get := &cobra.Command{
		Use:   "get <hash>",
		Short: "Write an object to stdout or to a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := cl().OpenObject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer goutils.CloseQuietly(body)
			if output == "" || output == "-" {
				_, err = io.Copy(cmd.OutOrStdout(), body)
				return err
			}
			f, err := os.Create(output)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, body); err != nil {
				_ = f.Close()
				_ = os.Remove(output)
				return err
			}
			return f.Close()
		},
	}
	get.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout")

	exists := &cobra.Command{
		Use:   "exists <hash>",
		Short: "Exit with 0 when the object exists, 1 when it does not",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ok, err := cl().HeadObject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !ok {
				return errAbsent
			}
			return nil
		},
	}

	var unusedBefore string
	del := &cobra.Command{
		Use:   "delete <hash>",
		Short: "Delete an object unless it was put at or after --unused-before",
		Long: "Delete an object unless it was put at or after --unused-before (RFC 3339 time, or a\n" +
			"duration such as 24h meaning that long ago). The server must allow deletes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			before, err := parseTime(unusedBefore, time.Now())
			if err != nil {
				return err
			}
			size, deleted, err := cl().DeleteObject(cmd.Context(), args[0], before)
			if err != nil {
				return err
			}
			state := "kept (put since then)"
			switch {
			case deleted:
				state = "deleted"
			case size == 0:
				state = "absent"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %d bytes\n", state, size)
			return err
		},
	}
	del.Flags().StringVar(&unusedBefore, "unused-before", "", "RFC 3339 time or duration ago (required)")
	_ = del.MarkFlagRequired("unused-before")

	labels := &cobra.Command{
		Use:   "labels <hash>",
		Short: "Print an object's labels as key=value lines",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := cl().Labels(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, k := range sortedKeys(m) {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", k, m[k]); err != nil {
					return err
				}
			}
			return nil
		},
	}

	label := &cobra.Command{Use: "label", Short: "Set or remove an object's labels"}
	label.AddCommand(&cobra.Command{
		Use:   "set <hash> <key>=<value>...",
		Short: "Set labels",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := parseLabels(args[1:], false)
			if err != nil {
				return err
			}
			c := cl()
			for _, k := range sortedKeys(m) {
				if err := c.SetLabel(cmd.Context(), args[0], k, m[k]); err != nil {
					return fmt.Errorf("label %s: %w", k, err)
				}
			}
			return nil
		},
	}, &cobra.Command{
		Use:   "delete <hash> <key>...",
		Short: "Remove labels (a missing label is not an error)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cl()
			for _, k := range args[1:] {
				if err := c.DeleteLabel(cmd.Context(), args[0], k); err != nil {
					return fmt.Errorf("label %s: %w", k, err)
				}
			}
			return nil
		},
	})

	find := &cobra.Command{
		Use:   "find <key>[=<value>]...",
		Short: "Print the hashes of the objects carrying every given label",
		Long:  "Print the hashes of the objects carrying every given label; a bare key matches any value.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sel, err := parseLabels(args, true)
			if err != nil {
				return err
			}
			hashes, err := cl().FindByLabels(cmd.Context(), sel)
			if err != nil {
				return err
			}
			for _, h := range hashes {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), h); err != nil {
					return err
				}
			}
			return nil
		},
	}

	root.AddCommand(put, get, exists, del, labels, label, find)
	return root
}

// parseLabels reads key=value arguments; with anyValue a bare key stands for
// any value ("").
func parseLabels(args []string, anyValue bool) (map[string]string, error) {
	m := make(map[string]string, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if k == "" || (!ok && !anyValue) || (ok && v == "") {
			return nil, fmt.Errorf("invalid label %q, want key=value", a)
		}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("label %q given twice", k)
		}
		m[k] = v
	}
	return m, nil
}

// parseTime reads an RFC 3339 time or a duration before now.
func parseTime(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("invalid time %q, want RFC 3339 or a duration such as 24h", s)
	}
	return now.Add(-d), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
