package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/pgombola/gomad/client"
)

type program struct {
	install  string
	hostname string
	nomad    *client.NomadServer
	launch   string
	exit     chan struct{}
	logger   service.Logger
}

func (p *program) Start(s service.Service) error {
	p.logger.Info("Starting Clarify")
	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	p.logger.Info("Stopping Clarify")
	p.exit <- struct{}{}
	node := p.node()
	status, err := client.Drain(p.nomad, node.ID, true)
	if err != nil {
		p.logger.Error("Error enabling node-drain.")
		return err
	}
	if status != http.StatusOK {
		p.logger.Errorf("Error enable node-drain; returned %v status code.", s)
		return errors.New("error enabling node-drain")
	}

	return nil
}

func (p *program) run() {
	waitForInstall(p.install, p.logger)
	if _, err := client.FindJob(p.nomad, "clarify"); err == nil {
		node := p.node()
		if node.Drain {
			p.disableDrain(node.ID)
		}
	} else {
		p.launchClarify()
	}
	stopped := make(chan struct{})
	done := p.pollJob(stopped)
	select {
	case <-stopped:
		os.Exit(1)
	case <-p.exit:
		close(done)
	}
}

func (p *program) pollJob(stopped chan<- struct{}) chan<- bool {
	done := make(chan bool)
	go func() {
		jobNotExists := func() bool {
			_, err := client.FindJob(p.nomad, "clarify")
			return err == nil
		}
		ticker := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-ticker.C:
				if jobNotExists() {
					p.logger.Error("Clarify job not found.")
					ticker.Stop()
					close(done)
					stopped <- struct{}{}
				}
			case <-done:
				ticker.Stop()
			}
		}
	}()
	return done
}

func (p *program) launchClarify() {
	s, err := client.SubmitJob(p.nomad, strings.Join([]string{p.install, p.launch}, string(filepath.Separator)))
	if err != nil {
		p.logger.Error("Error launching Clarify.")
		p.logger.Error(err)
		os.Exit(1)
	}
	if s != http.StatusOK {
		p.logger.Errorf("Error launching Clarify; returned %v status code.", s)
		os.Exit(1)
	}
}

func (p *program) node() *client.Host {
	hostname, err := os.Hostname()
	if err != nil {
		p.logger.Error("Unable to retrieve hostname")
		os.Exit(1)
	}
	node, err := client.HostID(p.nomad, &hostname)
	if err != nil {
		p.logger.Errorf("Error retrieving node")
		p.logger.Error(err)
		os.Exit(1)
	}
	return node
}

func (p *program) disableDrain(id string) {
	s, err := client.Drain(p.nomad, id, false)
	if err != nil {
		p.logger.Error("Error disabling drain.")
		p.logger.Error(err)
		os.Exit(1)
	}
	if s != http.StatusOK {
		p.logger.Errorf("Error disabling drain; returned %v status.", s)
		os.Exit(1)
	}
}

func waitForInstall(path string, log service.Logger) {
	shareNotExists := func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}
	for shareNotExists() {
		log.Warning("Share not mounted; waiting.")
		time.Sleep(10 * time.Second)
	}
}

func main() {
	control := flag.String("control", "", fmt.Sprintf("Service control command[%q].", service.ControlAction))
	install := flag.String("install", "", "The location of Clarify install directory.")
	port := flag.Int("nomadport", 4646, "Port of local Nomad instance.")
	launch := flag.String("launch", "launch_clarify.json", "Filename of Clarify job specification.")
	flag.Parse()

	if len(*install) == 0 {
		log.Fatal("install locaton must be provided")
	}

	// Program
	var prg *program
	{
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatal("error retrieving hostname")
		}
		prg = &program{
			install:  *install,
			hostname: hostname,
			nomad:    &client.NomadServer{Address: "localhost", Port: *port},
			launch:   *launch,
			exit:     make(chan struct{}),
		}
	}

	// Service
	var s service.Service
	{
		svcConfig := &service.Config{
			Name:         "Clarify",
			DisplayName:  "Clarify Service",
			Description:  "Controls the Clarify application.",
			Arguments:    []string{"-install", *install},
			Dependencies: []string{"clarify-consul, clarify-nomad"},
		}
		s, _ = service.New(prg, svcConfig)
	}

	// Logging
	var logger service.Logger
	{
		wd, _ := filepath.Abs(filepath.Dir(os.Args[0]))
		name := strings.Join([]string{wd, "clarifysvc.log"}, string(filepath.Separator))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {

		}
		defer f.Close()
		log.SetOutput(f)
		errs := make(chan error, 5)
		logger, _ = s.Logger(errs)
		go func() {
			for {
				err := <-errs
				if err != nil {
					log.Println(err)
				}
			}
		}()
		prg.logger = logger
	}

	// Run control command or start program
	if len(*control) != 0 {
		if err := service.Control(s, *control); err != nil {
			log.Fatalf("error running %s command", *control)
		}
		return
	}

	if err := s.Run(); err != nil {
		logger.Error(err)
	}
}
