package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/template"

	"github.com/go-git/go-git/v5"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"

	"github.com/twpayne/chezmoi/internal/chezmoi"
)

type initCmdConfig struct {
	apply       bool
	depth       int
	oneShot     bool
	purge       bool
	purgeBinary bool
}

var dotfilesRepoGuesses = []struct {
	rx     *regexp.Regexp
	format string
}{
	{
		rx:     regexp.MustCompile(`\A[-0-9A-Za-z]+\z`),
		format: "https://github.com/%s/dotfiles.git",
	},
	{
		rx:     regexp.MustCompile(`\A[-0-9A-Za-z]+/[-0-9A-Za-z]+\.git\z`),
		format: "https://github.com/%s",
	},
	{
		rx:     regexp.MustCompile(`\A[-0-9A-Za-z]+/[-0-9A-Za-z]+\z`),
		format: "https://github.com/%s.git",
	},
	{
		rx:     regexp.MustCompile(`\A[-.0-9A-Za-z]+/[-0-9A-Za-z]+\z`),
		format: "https://%s/dotfiles.git",
	},
	{
		rx:     regexp.MustCompile(`\A[-.0-9A-Za-z]+/[-0-9A-Za-z]+/[-0-9A-Za-z]+\z`),
		format: "https://%s.git",
	},
	{
		rx:     regexp.MustCompile(`\A[-.0-9A-Za-z]+/[-0-9A-Za-z]+/[-0-9A-Za-z]+\.git\z`),
		format: "https://%s",
	},
	{
		rx:     regexp.MustCompile(`\Asr\.ht/~[-0-9A-Za-z]+\z`),
		format: "https://git.%s/dotfiles",
	},
	{
		rx:     regexp.MustCompile(`\Asr\.ht/~[-0-9A-Za-z]+/[-0-9A-Za-z]+\z`),
		format: "https://git.%s",
	},
}

func (c *Config) newInitCmd() *cobra.Command {
	initCmd := &cobra.Command{
		Args:    cobra.MaximumNArgs(1),
		Use:     "init [repo]",
		Short:   "Setup the source directory and update the destination directory to match the target state",
		Long:    mustLongHelp("init"),
		Example: example("init"),
		RunE:    c.runInitCmd,
		Annotations: map[string]string{
			modifiesDestinationDirectory: "true",
			persistentStateMode:          persistentStateModeReadWrite,
			requiresSourceDirectory:      "true",
			runsCommands:                 "true",
		},
	}

	flags := initCmd.Flags()
	flags.BoolVarP(&c.init.apply, "apply", "a", c.init.apply, "update destination directory")
	flags.IntVarP(&c.init.depth, "depth", "d", c.init.depth, "create a shallow clone")
	flags.BoolVar(&c.init.oneShot, "one-shot", c.init.oneShot, "one shot")
	flags.BoolVarP(&c.init.purge, "purge", "p", c.init.purge, "purge config and source directories")
	flags.BoolVarP(&c.init.purgeBinary, "purge-binary", "P", c.init.purgeBinary, "purge chezmoi binary")

	return initCmd
}

func (c *Config) runInitCmd(cmd *cobra.Command, args []string) error {
	if c.init.oneShot {
		c.force = true
		c.init.apply = true
		c.init.depth = 1
		c.init.purge = true
	}

	// If the source repo does not exist then init or clone it.
	switch _, err := c.baseSystem.Stat(c.sourceDirAbsPath.Join(chezmoi.RelPath(".git"))); {
	case os.IsNotExist(err):
		rawSourceDir, err := c.baseSystem.RawPath(c.sourceDirAbsPath)
		if err != nil {
			return err
		}

		useBuiltinGit, err := c.useBuiltinGit()
		if err != nil {
			return err
		}

		if len(args) == 0 {
			if useBuiltinGit {
				isBare := false
				if _, err = git.PlainInit(string(rawSourceDir), isBare); err != nil {
					return err
				}
			} else if err := c.run(c.sourceDirAbsPath, c.Git.Command, []string{"init"}); err != nil {
				return err
			}
		} else {
			dotfilesRepoURL := guessDotfilesRepoURL(args[0])
			if useBuiltinGit {
				isBare := false
				if _, err := git.PlainClone(string(rawSourceDir), isBare, &git.CloneOptions{
					URL:               dotfilesRepoURL,
					Depth:             c.init.depth,
					RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
				}); err != nil {
					return err
				}
			} else {
				args := []string{
					"clone",
					"--recurse-submodules",
				}
				if c.init.depth != 0 {
					args = append(args,
						"--depth", strconv.Itoa(c.init.depth),
					)
				}
				args = append(args,
					dotfilesRepoURL,
					string(rawSourceDir),
				)
				if err := c.run("", c.Git.Command, args); err != nil {
					return err
				}
			}
		}
	case err != nil:
		return err
	}

	// Find config template, execute it, and create config file.
	configTemplateRelPath, ext, configTemplateContents, err := c.findConfigTemplate()
	if err != nil {
		return err
	}
	var configFileContents []byte
	if configTemplateRelPath == "" {
		if err := c.persistentState.Delete(chezmoi.ConfigStateBucket, configStateKey); err != nil {
			return err
		}
	} else {
		configFileContents, err = c.createConfigFile(configTemplateRelPath, configTemplateContents)
		if err != nil {
			return err
		}
		configStateValue, err := json.Marshal(configState{
			ConfigTemplateContentsSHA256: chezmoi.HexBytes(chezmoi.SHA256Sum(configTemplateContents)),
		})
		if err != nil {
			return err
		}
		if err := c.persistentState.Set(chezmoi.ConfigStateBucket, configStateKey, configStateValue); err != nil {
			return err
		}
	}

	// Reload config if it was created.
	if configTemplateRelPath != "" {
		viper.SetConfigType(ext)
		if err := viper.ReadConfig(bytes.NewBuffer(configFileContents)); err != nil {
			return err
		}
		if err := viper.Unmarshal(c); err != nil {
			return err
		}
	}

	// Apply.
	if c.init.apply {
		if err := c.applyArgs(c.destSystem, c.destDirAbsPath, noArgs, applyArgsOptions{
			include:      chezmoi.NewEntryTypeSet(chezmoi.EntryTypesAll),
			recursive:    false,
			umask:        c.Umask,
			preApplyFunc: c.defaultPreApplyFunc,
		}); err != nil {
			return err
		}
	}

	// Purge.
	if c.init.purge {
		if err := c.doPurge(&purgeOptions{
			binary: runtime.GOOS != "windows" && c.init.purgeBinary,
		}); err != nil {
			return err
		}
	}

	return nil
}

// createConfigFile creates a config file using a template and returns its
// contents.
func (c *Config) createConfigFile(filename chezmoi.RelPath, data []byte) ([]byte, error) {
	funcMap := make(template.FuncMap)
	chezmoi.RecursiveMerge(funcMap, c.templateFuncs)
	chezmoi.RecursiveMerge(funcMap, map[string]interface{}{
		"promptBool":   c.promptBool,
		"promptInt":    c.promptInt,
		"promptString": c.promptString,
		"stdinIsATTY":  c.stdinIsATTY,
	})

	t, err := template.New(string(filename)).Funcs(funcMap).Parse(string(data))
	if err != nil {
		return nil, err
	}

	sb := strings.Builder{}
	templateData := c.defaultTemplateData()
	chezmoi.RecursiveMerge(templateData, c.Data)
	if err = t.Execute(&sb, templateData); err != nil {
		return nil, err
	}
	contents := []byte(sb.String())

	configDir := chezmoi.AbsPath(c.bds.ConfigHome).Join("chezmoi")
	if err := chezmoi.MkdirAll(c.baseSystem, configDir, 0o777); err != nil {
		return nil, err
	}

	configPath := configDir.Join(filename)
	if err := c.baseSystem.WriteFile(configPath, contents, 0o600); err != nil {
		return nil, err
	}

	return contents, nil
}

func (c *Config) promptBool(field string) bool {
	value, err := parseBool(c.promptString(field))
	if err != nil {
		returnTemplateError(err)
		return false
	}
	return value
}

func (c *Config) promptInt(field string) int64 {
	value, err := strconv.ParseInt(c.promptString(field), 10, 64)
	if err != nil {
		returnTemplateError(err)
		return 0
	}
	return value
}

func (c *Config) promptString(field string) string {
	value, err := c.readLine(fmt.Sprintf("%s? ", field))
	if err != nil {
		returnTemplateError(err)
		return ""
	}
	return strings.TrimSpace(value)
}

func (c *Config) stdinIsATTY() bool {
	file, ok := c.stdin.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

// guessDotfilesRepoURL guesses the user's dotfile repo from arg.
func guessDotfilesRepoURL(arg string) string {
	for _, dotfileRepoGuess := range dotfilesRepoGuesses {
		if dotfileRepoGuess.rx.MatchString(arg) {
			return fmt.Sprintf(dotfileRepoGuess.format, arg)
		}
	}
	return arg
}
