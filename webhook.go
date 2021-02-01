package main

import (
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/dimfeld/glog"
	"github.com/dimfeld/goconfig"
	"os"
	"os/signal"
	"path/filepath"
	"text/template"
)

type Hook struct {
	// URL at which this hook should be available.
	Url string
	// Dir is the working directory from which the command should be run.
	// If blank, the current working directory is used.
	Dir string
	// Env is a list of environment variables to set. If empty, the current
	// environment is used. Each item takes the form "key=value"
	Env []string

	// If PerCommit is true, call the hook once for each commit in the message.
	// Otherwise it is just called once per message.
	PerCommit bool

	// If empty, all events are accepted.
	AllowEvent []string

	// If empty, all pipeline status ar accepted
	AllowPipelineStatus []string

	// Trigger the hook on changes to the following branches. If empty,
	// the hook does not match on a particular branch.
	AllowBranches []string

	// Commands to run.
	Commands [][]string

	// Override the default timeout.
	Timeout int

	// Secret required in the request. Requests that don't have a matching
	// Secret will be ignored. Note that Gitlab does not support this feature.
	// If specified, this overrides any server-wide secret.
	// If a secret is present in the server-wide configuration, it can be disabled for
	// this hook by setting the hook's secret to "none".
	Secret string

	cmdTemplate [][]*template.Template
	envTemplate []*template.Template
	dirTemplate *template.Template
}

type Hooks struct {
	Hook []*Hook
}

type Config struct {
	ListenAddress string

	LogDir string

	// The maximum amount of time to wait for a command to finish.
	// Default is 5 seconds.
	CommandTimeout int

	// Accept connections from only the given IP addresses.
	AcceptIps []string

	// Default secret required in requests. See the Hook struct for more description.
	Secret string

	// Paths to search for hook files
	HookPaths []string

	Hook []*Hook
}

func (c *Config) MergeHooks(other *Hooks) {
	c.Hook = append(c.Hook, other.Hook...)
}

func (c *Config) AddHookFile(file string) {
	var err error
	h := &Hooks{}

	f := os.Stdin

	if file == "-" {
		// Change file here so that any error messages will look better.
		file = "stdin"
	} else {
		f, err = os.Open(file)
		if err != nil {
			glog.Fatalf("Error loading %s: %s", file, err)
			return
		}
		defer f.Close()
	}

	glog.Infoln("Reading hooks from", file)

	_, err = toml.DecodeReader(f, h)
	if err != nil {
		glog.Fatalf("Error loading %s: %s", file, err)
		return
	}

	c.MergeHooks(h)
}

func (c *Config) AddHookPath(p string) {
	info, err := os.Stat(p)
	if err != nil {
		glog.Fatalf("Error loading %s: %s", p, err)
		return
	}

	if info.IsDir() {
		filepath.Walk(p,
			func(path string, info os.FileInfo, err error) error {
				if err != nil {
					glog.Fatalf("Error loading %s, %s", path, err)
					return err
				}
				if info.IsDir() {
					return nil
				}

				c.AddHookFile(path)
				return nil
			})
	} else {
		c.AddHookFile(p)
	}
}

func catchSIGINT(f func(), quit bool) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for _ = range c {
			glog.Info("SIGINT received...")
			f()
			if quit {
				os.Exit(1)
			}
		}
	}()
}

func isDirectory(dirPath string) bool {
	stat, err := os.Stat(dirPath)
	if err != nil || !stat.IsDir() {
		return false
	}
	return true
}

func main() {
	flag.Parse()

	config := &Config{
		ListenAddress:  ":80",
		CommandTimeout: 5,
	}

	mainConfigPath := os.Getenv("UNWEBHOOK_CONFFILE")
	hooksStartIndex := 0
	if mainConfigPath == "" {
		if flag.NArg() != 0 {
			mainConfigPath = flag.Arg(0)
			hooksStartIndex = 1
		} else {
			mainConfigPath = os.Args[0] + ".conf"
		}
	}

	if mainConfigPath == "-" {
		fmt.Fprintf(os.Stderr, "Loading main config from stdin")
		goconfig.Load(config, os.Stdin, "UNWEBHOOK")
	} else {
		f, err := os.Open(mainConfigPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open config file %s: %s\n",
				mainConfigPath, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Loading main config from %s", mainConfigPath)
		err = goconfig.Load(config, f, "UNWEBHOOK")
		f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading config file %s: %s\n",
				mainConfigPath, err)
			os.Exit(1)
		}
	}

	// Use config.LogDir if not given on the command line.
	dir := flag.CommandLine.Lookup("log_dir")
	if dir != nil && dir.Value.String() == "" {
		if config.LogDir == "" {
			config.LogDir = "."
		}
		flag.Set("log_dir", config.LogDir)

		if !isDirectory(config.LogDir) {
			err := os.MkdirAll(config.LogDir, 0755)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to create log directory: %s\n", err)
				fmt.Fprintf(os.Stderr, "Logs will go to $TMPDIR\n")
			}
		}
	}

	for _, h := range config.HookPaths {
		config.AddHookPath(h)
	}

	if flag.NArg() > hooksStartIndex {
		for _, arg := range flag.Args()[hooksStartIndex:] {
			config.AddHookPath(arg)
		}
	}

	closer := func() {
		glog.Flush()
	}
	catchSIGINT(closer, true)
	defer closer()

	failed := false
	for _, h := range config.Hook {
		glog.Infoln("Loading hook", h.Url)

		if h.Timeout == 0 {
			h.Timeout = config.CommandTimeout
		}

		if h.Secret == "none" {
			h.Secret = ""
		} else if h.Secret == "" {
			h.Secret = config.Secret
		}

		err := h.CreateTemplates()
		if err != nil {
			glog.Errorf("Failed parsing template %s: %s", h.Url, err)
			failed = true
		}
	}

	if failed {
		os.Exit(1)
	}

	RunServer(config)
}
