package refgen

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/thec1oud/galleon/cli"
)

// commandName is what a command's name may be: it is written unescaped,
// in a heading and a shell block.
var commandName = regexp.MustCompile(`^[a-z][a-z-]*( [a-z][a-z-]*)*$`)

// renderCLI renders the command line's reference from the commands'
// definitions, which also run them and print their help.
func renderCLI(commands []cli.Command, exits []cli.Exit) (string, error) {
	var b strings.Builder
	b.WriteString(`---
title: "Command line"
description: "Every command an app's binary runs through package cli, with its flags and exit codes."
order: 3
---

`)
	b.WriteString(generated)
	b.WriteString("# Command line\n\n")
	b.WriteString("An app whose `main` hands its arguments to [`cli.Main`](https://pkg.go.dev/github.com/thec1oud/galleon/cli#Main) runs these commands, as `<app> <command>`, where `<app>` is its binary. Without a command, it serves. Each command builds the app as `main` configures it, so it reads the same config file and environment, and its migrators and policy are the ones the app boots with ([0500](/docs/decisions/0500-commands-are-subcommands-of-the-apps-binary)).\n\n")
	b.WriteString("```go\nfunc main() {\n\tos.Exit(cli.Main(context.Background(), os.Args[1:], options()...))\n}\n```\n\n")
	b.WriteString("Reports go to stdout, and errors and usage to stderr. The app's logs go wherever its logger writes them, stderr by default. `<app> help` lists the commands, and `<app> help <command>`, or `-h` after a command, prints its flags and exit codes.\n\n")
	b.WriteString("## Exit codes\n\n| Code | Meaning |\n|---|---|\n")
	for _, e := range exits {
		fmt.Fprintf(&b, "| `%d` | %s |\n", e.Code, cell(sentence(e.Meaning)))
	}
	b.WriteString("\nEach command's section says what `0` and `1` mean for it.\n")
	for _, c := range commands {
		if !commandName.MatchString(c.Name) || c.Summary == "" || len(c.Exits) == 0 {
			return "", fmt.Errorf("refgen: command %q needs a name of lowercase words, a summary and exit codes", c.Name)
		}
		fmt.Fprintf(&b, "\n## %s\n\n```sh\n<app> %s", c.Name, c.Name)
		if c.Args != "" {
			fmt.Fprintf(&b, " %s", c.Args)
		}
		fmt.Fprintf(&b, "\n```\n\n%s\n", cell(c.Summary))
		if c.Doc != "" {
			for _, para := range strings.Split(c.Doc, "\n\n") {
				fmt.Fprintf(&b, "\n%s\n", cell(para))
			}
		}
		if len(c.Flags) > 0 {
			b.WriteString("\n| Flag | Default | What it does |\n|---|---|---|\n")
			for _, f := range c.Flags {
				fmt.Fprintf(&b, "| `-%s` | `%s` | %s |\n", f.Name, f.Default, cell(sentence(f.Usage)))
			}
		}
		b.WriteString("\n| Exit code | When |\n|---|---|\n")
		for _, e := range c.Exits {
			fmt.Fprintf(&b, "| `%d` | %s |\n", e.Code, cell(sentence(e.Meaning)))
		}
	}
	return b.String(), nil
}
