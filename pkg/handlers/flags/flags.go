package flags

import (
	"fmt"

	"github.com/spf13/cobra"
)

// FlagRetriever defines an interface for retrieving flags.
type FlagRetriever interface {
	GetString(name string) (string, error)
	GetBool(name string) (bool, error)
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
	return r.Cmd.Flags().GetString(name)
}

// GetBool retrieves a boolean flag from the cobra command.
func (r *CommandFlagRetriever) GetBool(name string) (bool, error) {
	return r.Cmd.Flags().GetBool(name)
}

func GetFlags(flagRetriever FlagRetriever, additionalFlags []Flag) (map[string]interface{}, error) {
	baseFlags := []Flag{
		{Name: "region", Type: "string"},
		{Name: "auth-method", Type: "string"},
		{Name: "profile", Type: "string"},
	}

	allFlags := append(baseFlags, additionalFlags...)
	results := make(map[string]interface{})

	for _, flag := range allFlags {
		var err error
		switch flag.Type {
		case "string":
			results[flag.Name], err = flagRetriever.GetString(flag.Name)
		case "bool":
			results[flag.Name], err = flagRetriever.GetBool(flag.Name)
		default:
			err = fmt.Errorf("unsupported flag type: %s", flag.Type)
		}
		if err != nil {
			return nil, err
		}
	}

	return results, nil
}
