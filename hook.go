package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/dimfeld/glog"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"
)

// Hook is defined in webhook.go.

var templateFuncs = template.FuncMap{
	"json": func(obj interface{}) string {
		result, err := json.Marshal(obj)
		if err != nil {
			return "<< " + err.Error() + " >>"
		}
		return string(result)
	},
}

func createTemplate(source string) (*template.Template, error) {
	source = os.ExpandEnv(source)
	return template.New("tmpl").Funcs(templateFuncs).Parse(source)
}

// CreateTemplates parses the Commands array into templates.
func (hook *Hook) CreateTemplates() error {
	var err error
	hook.cmdTemplate = make([][]*template.Template, len(hook.Commands))
	for i, cmdList := range hook.Commands {
		hook.cmdTemplate[i] = make([]*template.Template, len(cmdList))

		for j, cmd := range cmdList {

			hook.cmdTemplate[i][j], err = createTemplate(cmd)
			if err != nil {
				hook.cmdTemplate = nil
				return err
			}
		}
	}

	if len(hook.Env) != 0 {
		hook.envTemplate = make([]*template.Template, len(hook.Env))
		for i, env := range hook.Env {
			hook.envTemplate[i], err = createTemplate(env)
			if err != nil {
				return err
			}
		}
	} else {
		hook.envTemplate = nil
	}

	if hook.Dir != "" {
		hook.dirTemplate, err = createTemplate(hook.Dir)
		if err != nil {
			return err
		}
	} else {
		hook.dirTemplate = nil
	}

	return nil
}

// Execute a hook with the given event.
func (hook *Hook) Execute(e Event) {
	if len(hook.AllowEvent) != 0 {
		eventType, ok := e["type"].(string)
		if !ok {
			glog.Warningf("Received non-string event type %T: %v", eventType, eventType)
			return
		}

		allowed := false
		for _, allowedEvent := range hook.AllowEvent {
			if allowedEvent == eventType {
				allowed = true
				break
			}
		}

		if !allowed {
			glog.Warningf("Hook %s got disallowed event type %s\n", hook.Url, eventType)
			return
		}
	}
	if len(hook.AllowPipelineStatus) != 0 {
		pipelineStatus, ok := e["status"].(string)
		if !ok {
			glog.Warningf("Received non-string Pipeline Status %T: %v", pipelineStatus, pipelineStatus)
			return
		}

		allowed := false
		for _, allowPipelineStatus := range hook.AllowPipelineStatus {
			if allowPipelineStatus == pipelineStatus {
				allowed = true
				break
			}
		}

		if !allowed {
			glog.Infof("Hook %s called for incorrect pipeline status %s\n", hook.Url, pipelineStatus)
			return
		}
	}

	if len(hook.AllowBranches) != 0 {
		ref, ok := e["ref"].(string)
		if !ok {
			glog.Warningf("Received non-string ref type %T: %v", ref, ref)
			return
		}

		// Strip off refs/heads, if present.
		prefixString := "refs/heads/"
		if strings.HasPrefix(ref, prefixString) {
			ref = ref[len(prefixString):]
		}

		allowed := false
		for _, allowedBranch := range hook.AllowBranches {
			if ref == allowedBranch {
				allowed = true
				break
			}
		}

		if !allowed {
			// This is just an Info, not a warning, since there's no way
			// to configure Github or Gitlab to only send events for certain
			// branches.
			glog.Infof("Hook %s called for ignored branch %s\n", hook.Url, ref)
			return
		}
	}

	if hook.PerCommit {
		commits := e.Commits()
		if commits != nil {
			for _, generic := range commits {
				c, ok := generic.(map[string]interface{})
				if !ok {
					glog.Errorf("Commit had type %T", generic)
					continue
				}

				// Set the current commit to pass to the hook.
				e["commit"] = c

				err := hook.processEvent(e)
				if err != nil {
					glog.Errorf("Error processing %s: %s\n", hook.Url, err)
					if glog.V(1) {
						glog.Info(e)
					}
				}
			}
		}
	} else {
		err := hook.processEvent(e)
		if err != nil {
			glog.Errorf("Error processing %s: %s\n", hook.Url, err)
			if glog.V(1) {
				glog.Info(e)
			}
		}
	}
}

func (hook *Hook) processEvent(e Event) error {
	var err error
	cmds := make([][]string, len(hook.cmdTemplate))
	env := make([]string, len(hook.envTemplate))
	dir := ""

	if hook.dirTemplate != nil {
		buf := &bytes.Buffer{}
		err = hook.dirTemplate.Execute(buf, e)
		dir = string(buf.Bytes())
		if err != nil {
			return err
		}
	}

	if hook.envTemplate != nil {
		for i, t := range hook.envTemplate {
			buf := &bytes.Buffer{}
			err = t.Execute(buf, e)
			env[i] = string(buf.Bytes())
			if err != nil {
				return err
			}
		}
	}

	for i, t := range hook.cmdTemplate {
		cmds[i], err = hook.processCommand(e, t)
		if err != nil {
			return err
		}

		execPath, err := exec.LookPath(cmds[i][0])
		if err != nil {
			return fmt.Errorf("Executable %s %s", cmds[i][0], err)
		}
		cmds[i][0] = execPath
	}

	for _, cmd := range cmds {
		err := hook.runCommand(cmd, env, dir)
		if err != nil {
			return err
		}
	}

	return nil
}

func (hook *Hook) processCommand(e Event, templateList []*template.Template) ([]string, error) {
	cmdList := make([]string, len(templateList))

	for i, t := range templateList {
		cmd := &bytes.Buffer{}

		err := t.Execute(cmd, e)
		if err != nil {
			return nil, err
		}

		cmdList[i] = string(cmd.Bytes())
	}

	return cmdList, nil
}

func (hook *Hook) runCommand(args []string, env []string, dir string) error {
	glog.Infoln("Running", args)
	cmd := exec.Command(args[0], args[1:]...)
	if len(env) != 0 {
		cmd.Env = env
	}
	cmd.Dir = dir
	// TODO Make these redirectable
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	done := make(chan int, 1)

	cmd.Start()
	go func() {
		cmd.Wait()
		done <- 1
	}()

	timer := time.NewTimer(time.Duration(hook.Timeout) * time.Second)

	select {
	case <-done:
		timer.Stop()
		return nil

	case <-timer.C:
		cmd.Process.Kill()
		return fmt.Errorf("Command %v timed out", args)
	}

}
