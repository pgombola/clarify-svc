package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/kardianos/service"
)

type programWatcher interface {
	watch(chan<- programExit)
}

type programExit struct {
	prg *program
	err error
}

type program struct {
	name   string
	exe    string
	config string
	args   []string
	cmd    *exec.Cmd
}

type clarify struct {
	consul  *program
	nomad   *program
	logger  service.Logger
	verbose *bool
}

func (p *program) watch(done chan<- programExit) {
	go func() {
		err := p.cmd.Wait()
		done <- programExit{
			err: err,
			prg: p,
		}
	}()
}

func (c *clarify) Start(s service.Service) error {
	c.logger.Infof("Starting Clarify\n(exe=%s;config=%s)\n(exe=%s;config=%s)",
		c.consul.exe, c.consul.config,
		c.nomad.exe, c.nomad.config)
	c.consul.cmd = exec.Command(c.consul.exe, c.consul.args...)
	c.nomad.cmd = exec.Command(c.nomad.exe, c.nomad.args...)
	if *c.verbose {
		c.nomad.cmd.Stdout = os.Stdout
		c.nomad.cmd.Stderr = os.Stderr
		c.consul.cmd.Stdout = os.Stdout
		c.consul.cmd.Stderr = os.Stderr
	}
	go c.run()
	return nil
}

func (c *clarify) Stop(s service.Service) error {
	c.logger.Info("Stopping Clarify")
	// https://github.com/golang/go/issues/6720
	if runtime.GOOS == "windows" {
		if err := c.consul.cmd.Process.Kill(); err != nil {
			c.logger.Errorf("Error terminating consul:\n%v", err)
		}
		if err := c.nomad.cmd.Process.Kill(); err != nil {
			c.logger.Errorf("Error terminating nomad:\n%v", err)
		}
	} else {
		c.logger.Info("Sending Consul process interrupt.")
		if err := c.consul.cmd.Process.Signal(os.Interrupt); err != nil {
			c.logger.Errorf("Error interrupting consul:\n%v", err)
		}
		if err := c.nomad.cmd.Process.Signal(os.Interrupt); err != nil {
			c.logger.Errorf("Error interrupting nomad:\n%v", err)
		}
	}
	return nil
}

func (c *clarify) run() {
	c.consul.cmd.Start()
	c.nomad.cmd.Start()
	done := make(chan programExit, 1)
	c.consul.watch(done)
	c.nomad.watch(done)
	select {
	case exit := <-done:
		switch exit.err.(type) {
		case *exec.ExitError:
			c.logger.Errorf("%s process exited:\n%v", exit.prg.name, exit.err)
		default:
			c.logger.Infof("%s process exited gracefully.", exit.prg.name)
		}
	}
}

func findFile(dir string, name string) (result string, err error) {
	err = filepath.Walk(dir,
		filepath.WalkFunc(func(fp string, fi os.FileInfo, _ error) error {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			if matched, err := path.Match(name, fi.Name()); err != nil {
				return err
			} else if matched {
				result = fp
				return io.EOF
			}
			return nil
		}))
	if err == io.EOF {
		err = nil
	}
	return
}

func main() {
	control := flag.String("control", "", fmt.Sprintf("Service control command [%q].", service.ControlAction))
	verbose := flag.Bool("v", false, "Logs verbose output from the Consul process to consul.")
	flag.Parse()

	svcConfig := &service.Config{
		Name:        "Clarify",
		DisplayName: "Clarify Service",
		Description: "This service starts Clarify.",
	}

	wd, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}

	var c *program
	{
		exe, _ := findFile(wd, "consul*")
		config, _ := findFile(strings.Join([]string{wd, "consul", "conf"}, string(filepath.Separator)), "config.json")
		c = &program{
			name:   "Consul",
			exe:    exe,
			config: config,
			args:   []string{"agent", "-config-file", config},
		}
	}

	var n *program
	{
		exe, _ := findFile(wd, "nomad*")
		config, _ := findFile(strings.Join([]string{wd, "nomad", "conf"}, string(filepath.Separator)), "nomad.hcl")
		n = &program{
			name:   "Nomad",
			exe:    exe,
			config: config,
			args:   []string{"agent", "-config", config},
		}
	}

	clarify := &clarify{
		verbose: verbose,
		consul:  c,
		nomad:   n,
	}
	s, err := service.New(clarify, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	logger, err := s.Logger(nil)
	if err != nil {
		log.Fatal(err)
	}
	clarify.logger = logger

	if len(*control) > 0 {
		err := service.Control(s, *control)
		if err != nil {
			logger.Error(err)
		}
		return
	}
	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigchan:
			clarify.Stop(s)
		}
	}()
	if err := s.Run(); err != nil {
		logger.Error(err)
	}
}
