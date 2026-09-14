package cmd

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Prompts exist only on a TTY. Every one reads a whole line; EOF is an answer
// of "no" (or an error where there is no safe answer).

func (a *app) readLine() (string, error) {
	line, err := a.in.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// choose asks for a number in 1..n. There is no default: Enter alone asks
// again, because a preselected answer is a guess the person did not make.
func (a *app) choose(prompt string, n int) (int, error) {
	for {
		fmt.Fprintf(a.stdout, "%s [1-%d]: ", prompt, n)
		line, err := a.readLine()
		if err != nil {
			return 0, fail(exitUsage, "no_answer", "no answer given")
		}
		if v, err := strconv.Atoi(line); err == nil && v >= 1 && v <= n {
			return v, nil
		}
		if line != "" {
			fmt.Fprintf(a.stdout, "  please answer a number from 1 to %d\n", n)
		}
	}
}

// ask reads a value, with a default shown in brackets when def is not empty.
func (a *app) ask(prompt, def string) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(a.stdout, "%s [%s]: ", prompt, def)
		} else {
			fmt.Fprintf(a.stdout, "%s: ", prompt)
		}
		line, err := a.readLine()
		if err != nil {
			return "", fail(exitUsage, "no_answer", "no answer given")
		}
		if line == "" {
			if def != "" {
				return def, nil
			}
			continue
		}
		return line, nil
	}
}

// confirm is a yes/no question whose default is NO.
func (a *app) confirm(prompt string) bool {
	fmt.Fprintf(a.stdout, "%s [y/N]: ", prompt)
	line, err := a.readLine()
	if err != nil {
		return false
	}
	return oneOf(line, "y", "yes")
}
