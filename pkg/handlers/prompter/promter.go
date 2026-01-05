/*
Copyright 2024 Elastic Scaler Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
