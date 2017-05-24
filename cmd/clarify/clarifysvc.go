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
		p.logger.Error("error enabling node-drain.")
		return err
	}
	if status != http.StatusOK {
		p.logger.Errorf("error enable node-drain; returned %v status code.", s)
		return errors.New("error enabling node-drain")
	}
	return nil
}

func (p *program) run() {
	if found := waitForInstall(p.clarify, p.exit, p.logger); !found {
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
			os.Exit(1)
		}
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
	go func(done <-chan bool) {
		ticker := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-ticker.C:
				if _, err := client.FindJob(p.nomad, "clarify"); err != nil {
					p.logger.Error("clarify job not found")
					ticker.Stop()
					stopped <- struct{}{}
				}
				if _, err := client.HostID(p.nomad, &p.hostname); err != nil {
					p.logger.Info("node drained")
					ticker.Stop()
					stopped <- struct{}{}
				}
			case <-done:
				ticker.Stop()
			}
		}
	}(done)
	return done
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
		os.Exit(1)
	}
	if s != http.StatusOK {
		p.logger.Errorf("error disabling drain; returned %v status", s)
		os.Exit(1)
	}
}

func waitForInstall(path string, exit <-chan struct{}, log service.Logger) bool {
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		log.Info("found clarify install directory")
		return true
	}
	found := make(chan bool)
	defer close(found)
	go func(found chan<- bool) {
		ticker := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-ticker.C:
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					ticker.Stop()
					found <- true
					return
				}
				log.Warning("clarify install not available; waiting")
			case <-exit:
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
			Name:         "Clarify2",
			DisplayName:  "Clarify Service",
			Description:  "Clarify",
			Arguments:    []string{fmt.Sprintf("-clarify=%v", *clarify)},
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
			log.Fatal(err)
		}
		return
	}

	if err := s.Run(); err != nil {
		logger.Error(err)
	}
}
