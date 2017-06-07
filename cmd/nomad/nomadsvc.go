package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/service"
)

type nomad struct {
	logger  service.Logger
	verbose *bool
	path    string
	data    string
	config  string
	cmd     *exec.Cmd
	exit    chan struct{}
}

func (p *nomad) Start(s service.Service) error {
	p.logger.Infof("Starting Clarify-Nomad(exe=%s,config=%s)", p.path, p.config)
	p.cmd = exec.Command(p.path, "agent", fmt.Sprintf("-config=%s", p.config), fmt.Sprintf("-data-dir=%s", p.data))
	if *p.verbose {
		p.cmd.Stdout = os.Stdout
		p.cmd.Stderr = os.Stderr
	}
	go p.run()
	return nil
}

func (p *nomad) Stop(s service.Service) error {
	p.logger.Info("Stopping Clarify-Nomad")
	close(p.exit)
	// https://github.com/golang/go/issues/6720
	if runtime.GOOS == "windows" {
		if err := p.cmd.Process.Kill(); err != nil {
			p.logger.Errorf("Error terminating nomad:\n%v", err)
		}
	} else {
		p.logger.Info("Sending Nomad process interrupt.")
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			p.logger.Errorf("Error interrupting nomad:\n%v", err)
		}
	}
	return nil
}

func (p *nomad) run() {
	p.cmd.Start()
	done := wait(p.cmd)
	select {
	// The consul child process has exited
	case err := <-done:
		switch err.(type) {
		case *exec.ExitError:
			p.logger.Errorf("Nomad process exited:\n%v", err)
			os.Exit(1)
		default:
			p.logger.Info("Nomad process exited gracefully.")
			os.Exit(1)
		}
	case <-p.exit:
		return
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

func cleanup(data string) {
	// Remove data/client/alloc directory: http://github.com/hashicorp/nomad/issues/2560
	allocDir := strings.Join([]string{data, "client", "alloc"}, string(os.PathSeparator))
	if err := os.RemoveAll(allocDir); err != nil {
		log.Fatalf("unable to remove alloc dir (%s)", allocDir)
	}
	// We remove client-id and secret-id to force nomad to think we're a new client
	clientID := strings.Join([]string{data, "client", "client-id"}, string(os.PathSeparator))
	if _, err := os.Stat(clientID); err == nil {
		if err := os.Remove(clientID); err != nil {
			log.Fatalf("unable to remove client-id (%s)", clientID)
		}
	}
	secretID := strings.Join([]string{data, "client", "secret-id"}, string(os.PathSeparator))
	if _, err := os.Stat(secretID); err == nil {
		if err := os.Remove(secretID); err != nil {
			log.Fatalf("unable to remove secret-id (%s)", secretID)
		}
	}
}

func main() {
	control := flag.String("control", "", fmt.Sprintf("Service control command [%q].", service.ControlAction))
	cfg := flag.String("cfg", "config.hcl", "The name of the Nomad configuration file.")
	verbose := flag.Bool("v", false, "Logs verbose output from the Nomad process.")
	flag.Parse()

	// Program
	var prg *nomad
	{
		wd, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			log.Fatal(err)
		}
		exe, _ := findFile(wd, "nomad*")
		config, _ := findFile(wd, *cfg)
		data := strings.Join([]string{wd, "data"}, string(os.PathSeparator))
		cleanup(data)
		prg = &nomad{
			path:    exe,
			verbose: verbose,
			config:  config,
			data:    data,
			exit:    make(chan struct{}, 1),
		}
	}

	// Service
	var s service.Service
	{
		svcConfig := &service.Config{
			Name:         "clarify-nomad",
			DisplayName:  "clarify-nomad",
			Description:  "clarify-nomad service",
			Arguments:    []string{"-cfg", *cfg},
			Dependencies: []string{"clarify-consul"},
		}
		s, _ = service.New(prg, svcConfig)
	}

	// Logging
	var logger service.Logger
	{
		var err error
		logger, err = s.Logger(nil)
		if err != nil {
			log.Fatal(err)
		}
		prg.logger = logger
	}

	// Run control command or start program
	if len(*control) != 0 {
		if err := service.Control(s, *control); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := s.Run(); err != nil {
		logger.Error(err)
	}
}
