package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/pgombola/gomad/client"
)

type program struct {
	clarify  string
	hostname string
	nomad    *client.NomadServer
	launch   string
	exit     chan struct{}
	logger   service.Logger
	svc      service.Service
}

func (p *program) Start(s service.Service) error {
	p.logger.Info("Starting Clarify")
	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	close(p.exit)
	node := p.node()
	status, err := client.Drain(p.nomad, node.ID, true)
	if err != nil {
		p.logger.Error("error enabling node-drain.")
		return err
	}
	if status != http.StatusOK {
		p.logger.Errorf("error enable node-drain; returned %v status code.", s)
		return errors.New("error enabling node-drain")
	}
	p.logger.Info("Stopped Clarify")
	return nil
}

func (p *program) run() {
	if found := p.waitForInstall(); !found {
		p.logger.Error("clarify install not available")
		return
	}
	_, err := client.FindJob(p.nomad, "clarify")
	if err == nil {
		p.logger.Info("clarify found")
		node := p.node()
		if node.Drain {
			p.logger.Info("disabling drain")
			p.disableDrain(node.ID)
		}
		p.logger.Infof("drain disabled (name=%s;id=%s)", node.Name, node.ID)
	} else {
		p.logger.Info("launching clarify")
		_, err := p.launchClarify()
		if err != nil {
			p.logger.Error(err)
			// Exit will allow the service to restart
			os.Exit(1)
		}
	}
	stopped := p.pollJob()
	select {
	case <-stopped:
		p.svc.Stop()
	}
}

func (p *program) pollJob() <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-ticker.C:
				if _, err := client.FindJob(p.nomad, "clarify"); err != nil {
					p.logger.Error("clarify job not found")
					ticker.Stop()
					close(stopped)
				}
				n, err := client.HostID(p.nomad, &p.hostname)
				if err != nil {
					p.logger.Warning("error retrieving node")
				} else if n.Drain {
					p.logger.Info("node drained")
					ticker.Stop()
					close(stopped)
				}
			}
		}
	}()
	return stopped
}

func (p *program) launchClarify() (bool, error) {
	s, err := client.SubmitJob(p.nomad, strings.Join([]string{p.clarify, p.launch}, string(filepath.Separator)))
	if err != nil {
		return false, err
	}
	if s != http.StatusOK {
		return false, fmt.Errorf("http status: %v", s)
	}
	return true, nil
}

func (p *program) node() *client.Host {
	hostname, err := os.Hostname()
	if err != nil {
		p.logger.Error("unable to retrieve hostname")
		os.Exit(1)
	}
	node, err := client.HostID(p.nomad, &hostname)
	if err != nil {
		p.logger.Errorf("error retrieving node")
		p.logger.Error(err)
		os.Exit(1)
	}
	return node
}

func (p *program) disableDrain(id string) {
	s, err := client.Drain(p.nomad, id, false)
	if err != nil {
		p.logger.Error("error disabling drain")
		p.logger.Error(err)
	}
	if s != http.StatusOK {
		p.logger.Errorf("error disabling drain; returned %v status", s)
	}
}

func (p *program) waitForInstall() bool {
	if _, err := os.Stat(p.clarify); !os.IsNotExist(err) {
		p.logger.Info("found clarify install directory")
		return true
	}
	found := make(chan bool)
	defer close(found)
	go func(found chan<- bool) {
		ticker := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-ticker.C:
				if _, err := os.Stat(p.clarify); !os.IsNotExist(err) {
					ticker.Stop()
					found <- true
					return
				}
				p.logger.Warning("clarify install not available; waiting")
			case <-p.exit:
				ticker.Stop()
				found <- false
				return
			}
		}
	}(found)
	select {
	case f := <-found:
		return f
	}
}

func isInstall(control *string) bool {
	return len(*control) != 0 && *control == "install"
}

func main() {
	control := flag.String("control", "", fmt.Sprintf("Service control command %q.", service.ControlAction))
	clarify := flag.String("clarify", "", "The location of Clarify install directory.")
	nomad := flag.String("nomad", ":4646", "Address:Port of Nomad instance.")
	launch := flag.String("launch", "launch_clarify.json", "Filename of Clarify job specification.")

	flag.Parse()

	if (isInstall(control) || len(*control) == 0) && len(*clarify) == 0 {
		log.Fatal("clarify locaton must be provided")
	}

	// Program
	var prg *program
	{
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatal("error retrieving hostname")
		}
		addressPort := strings.Split(*nomad, ":")
		if len(addressPort[0]) == 0 {
			addressPort[0] = "localhost"
		}
		port, _ := strconv.Atoi(addressPort[1])
		prg = &program{
			clarify:  *clarify,
			hostname: hostname,
			nomad:    &client.NomadServer{Address: addressPort[0], Port: port},
			launch:   *launch,
			exit:     make(chan struct{}),
		}
	}

	// Service
	var s service.Service
	{
		svcConfig := &service.Config{
			Name:         "clarify",
			DisplayName:  "clarify",
			Description:  "clarify service",
			Arguments:    []string{fmt.Sprintf("-clarify=%v", *clarify)},
			Dependencies: []string{"clarify-consul", "clarify-nomad"},
		}
		s, _ = service.New(prg, svcConfig)
		prg.svc = s
	}

	// Logging
	var logger service.Logger
	{
		logger, _ = s.Logger(nil)
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
