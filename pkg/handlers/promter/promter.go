package promter

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

	return &promter{Promter: p}
}

type promter struct {
	Promter *consolePrompterConfig
}

func (p *promter) Confirm(prompt string) (*bool, error) {
	if _, err := fmt.Fprint(p.Promter.output, prompt); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(p.Promter.input)
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
