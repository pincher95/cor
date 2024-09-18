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
	fmt.Fprint(p.Promter.output, prompt)
	reader := bufio.NewReader(p.Promter.input)
	response, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	response = strings.ToLower(strings.TrimSpace(response))
	if response == "yes" || response == "y" {
		result := true
		return &result, nil
	} else if response == "no" || response == "n" {
		result := false
		return &result, nil
	}

	return nil, nil
}
