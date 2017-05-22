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
	"syscall"

	"github.com/kardianos/service"
)

type consul struct {
	logger  service.Logger
	verbose *bool
	path    string
	config  string
	cmd     *exec.Cmd
}

func (p *consul) Start(s service.Service) error {
	p.logger.Infof("Starting Clarify-Consul\n(exe=%s,config=%s)", p.path, p.config)
	p.cmd = exec.Command(p.path, "agent", "-config-file", p.config)
	if *p.verbose {
		p.cmd.Stdout = os.Stdout
		p.cmd.Stderr = os.Stderr
	}
	go p.run()
	return nil
}

func (p *consul) Stop(s service.Service) error {
	p.logger.Info("Stopping Clarify-Consul")
	// https://github.com/golang/go/issues/6720
	if runtime.GOOS == "windows" {
		if err := p.cmd.Process.Kill(); err != nil {
			p.logger.Errorf("Error terminating consul:\n%v", err)
		}
	} else {
		p.logger.Info("Sending Consul process interrupt.")
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			p.logger.Errorf("Error interrupting consul:\n%v", err)
		}
	}
	return nil
}

func (p *consul) run() {
	p.cmd.Start()
	done := wait(p.cmd)
	select {
	// The consul child process has exited
	case err := <-done:
		switch err.(type) {
		case *exec.ExitError:
			p.logger.Errorf("Consul process exited:\n%v", err)
			os.Exit(1)
		default:
			p.logger.Info("Consul process exited gracefully.")
			os.Exit(0)
		}
	}
}

func wait(cmd *exec.Cmd) chan error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	return done
}

func findFile(dir string, name string) (result string, err error) {
	err = filepath.Walk(dir,
		filepath.WalkFunc(func(fp string, fi os.FileInfo, _ error) error {
			if fi.IsDir() {
				return nil
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
	cfg := flag.String("cfg", "config.json", "The name of the Consul configuration file.")
	verbose := flag.Bool("v", false, "Logs verbose output from the Consul process to consul.")
	flag.Parse()

	svcConfig := &service.Config{
		Name:        "Clarify-Consul",
		DisplayName: "Clarify Consul Service",
		Description: "This service starts Consul for use by Clarify.",
	}

	wd, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}

	exe, _ := findFile(wd, "consul*")
	config, _ := findFile(wd, *cfg)

	prg := &consul{
		path:    exe,
		verbose: verbose,
		config:  config,
	}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	logger, err := s.Logger(nil)
	if err != nil {
		log.Fatal(err)
	}
	prg.logger = logger

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
			prg.Stop(s)
		}
	}()
	if err := s.Run(); err != nil {
		logger.Error(err)
	}
}
