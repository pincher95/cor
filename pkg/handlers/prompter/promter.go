package prompter

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type Client interface {
	Confirm(prompt string) (*bool, error)
}

type consolePrompterConfig struct {
	input  io.Reader
	output io.Writer
}

func NewConsolePrompter(input io.Reader, output io.Writer) Client {
	p := &consolePrompterConfig{
		input:  input,
		output: output,
	}

	return &prompter{Prompter: p}
}

type prompter struct {
	Prompter *consolePrompterConfig
}

func (p *prompter) Confirm(prompt string) (*bool, error) {
	if _, err := fmt.Fprint(p.Prompter.output, prompt); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(p.Prompter.input)
	response, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	response = strings.ToLower(strings.TrimSpace(response))

	switch response {
	case "yes", "y":
		result := true
		return &result, nil
	case "no", "n":
		result := false
		return &result, nil
	default:
		return nil, nil
	}
}
