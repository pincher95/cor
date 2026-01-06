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

package flags

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// FlagRetriever defines an interface for retrieving flags.
type FlagRetriever interface {
	GetString(name string) (string, error)
	GetBool(name string) (bool, error)
	// IsChanged returns true if a flag value was explicitly provided on the CLI.
	// This is used to ensure correct precedence: CLI > config file > env > defaults.
	IsChanged(name string) bool
}

// CommandFlagRetriever is a wrapper around *cobra.Command to implement FlagRetriever.
type CommandFlagRetriever struct {
	Cmd *cobra.Command
}

// Flag represents a flag with its name and type.
type Flag struct {
	Name string
	Type string // "string" or "bool"
}

// GetString retrieves a string flag from the cobra command.
func (r *CommandFlagRetriever) GetString(name string) (string, error) {
	// Prefer local flags, then inherited (persistent from parents), then persistent on this cmd.
	if r.Cmd.Flags().Lookup(name) != nil {
		return r.Cmd.Flags().GetString(name)
	}
	if r.Cmd.InheritedFlags().Lookup(name) != nil {
		return r.Cmd.InheritedFlags().GetString(name)
	}
	return r.Cmd.PersistentFlags().GetString(name)
}

// GetBool retrieves a boolean flag from the cobra command.
func (r *CommandFlagRetriever) GetBool(name string) (bool, error) {
	if r.Cmd.Flags().Lookup(name) != nil {
		return r.Cmd.Flags().GetBool(name)
	}
	if r.Cmd.InheritedFlags().Lookup(name) != nil {
		return r.Cmd.InheritedFlags().GetBool(name)
	}
	return r.Cmd.PersistentFlags().GetBool(name)
}

func (r *CommandFlagRetriever) IsChanged(name string) bool {
	// pflag.FlagSet.Changed(name) returns false if the flag is not defined in that set,
	// so we can safely check all relevant sets.
	return r.Cmd.Flags().Changed(name) || r.Cmd.InheritedFlags().Changed(name) || r.Cmd.PersistentFlags().Changed(name)
}

func GetFlags(flagRetriever FlagRetriever, additionalFlags []Flag) (*map[string]any, error) {
	baseFlags := []Flag{
		{Name: "region", Type: "string"},
		{Name: "auth-method", Type: "string"},
		{Name: "profile", Type: "string"},
		{Name: "delete", Type: "bool"},
		{Name: "sort-by", Type: "string"},
		{Name: "sort-desc", Type: "bool"},
	}

	allFlags := append(baseFlags, additionalFlags...)
	results := make(map[string]any, len(allFlags))

	for _, flag := range allFlags {
		var err error
		switch flag.Type {
		case "string":
			// Precedence: CLI > config/env (viper) > defaults
			if !flagRetriever.IsChanged(flag.Name) && viper.IsSet(flag.Name) {
				results[flag.Name] = viper.GetString(flag.Name)
				break
			}
			results[flag.Name], err = flagRetriever.GetString(flag.Name)
		case "bool":
			if !flagRetriever.IsChanged(flag.Name) && viper.IsSet(flag.Name) {
				results[flag.Name] = viper.GetBool(flag.Name)
				break
			}
			results[flag.Name], err = flagRetriever.GetBool(flag.Name)
		default:
			err = fmt.Errorf("unsupported flag type: %s", flag.Type)
		}
		if err != nil {
			return nil, err
		}
	}

	return &results, nil
}
