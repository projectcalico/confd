package template

import (
	"fmt"
	"sync"
	"time"

	"github.com/kelseyhightower/confd/log"
)

type Processor interface {
	Process()
}

func Process(config Config) error {
	// Get the template resources.
	ts, err := getTemplateResources(config)
	if err != nil {
		return err
	}

	// Configure the client with the set of prefixes.
	if err := setClientPrefixes(config, ts); err != nil {
		return err
	}

	var lastErr error
	for _, t := range ts {
		if err := t.process(); err != nil {
			log.Error(err.Error())
			lastErr = err
		}
	}
	return lastErr
}

// Called to notify the client which prefixes will be monitored.
func setClientPrefixes(config Config, trs []*TemplateResource) error {
	prefixes := []string{}

	// Loop through the full set of template resources and get a complete set of
	// unique prefixes that are being watched.
	pmap := map[string]bool{}
	for _, tr := range trs {
		for _, pk := range tr.PrefixedKeys {
			pmap[pk] = true
		}
	}
	for p, _ := range pmap {
		prefixes = append(prefixes, p)
	}

	// Tell the client the set of prefixes.
	return config.StoreClient.SetPrefixes(prefixes)
}

type watchProcessor struct {
	config   Config
	stopChan chan bool
	doneChan chan bool
	errChan  chan error
	wg       sync.WaitGroup
}

func WatchProcessor(config Config, stopChan, doneChan chan bool, errChan chan error) Processor {
	var wg sync.WaitGroup
	return &watchProcessor{config, stopChan, doneChan, errChan, wg}
}

func (p *watchProcessor) Process() {
	defer close(p.doneChan)
	// Get the set of template resources.
	ts, err := getTemplateResources(p.config)
	if err != nil {
		log.Fatal(err.Error())
		return
	}

	// Configure the client with the set of prefixes.
	if err := setClientPrefixes(p.config, ts); err != nil {
		log.Fatal(err.Error())
		return
	}

	// Start the individual watchers for each template.
	for _, t := range ts {
		t := t
		p.wg.Add(1)
		go p.monitorPrefix(t)
	}
	p.wg.Wait()
}

func (p *watchProcessor) monitorPrefix(t *TemplateResource) {
	defer p.wg.Done()
	for {
		index, err := t.storeClient.WatchPrefix(t.Prefix, t.PrefixedKeys, t.lastIndex, p.stopChan)
		if err != nil {
			p.errChan <- err
			// Prevent backend errors from consuming all resources.
			time.Sleep(time.Second * 2)
			continue
		}
		t.lastIndex = index
		if err := t.process(); err != nil {
			p.errChan <- err
		}
	}
}

func getTemplateResources(config Config) ([]*TemplateResource, error) {
	var lastError error
	templates := make([]*TemplateResource, 0)
	log.Debug("Loading template resources from confdir " + config.ConfDir)
	if !isFileExist(config.ConfDir) {
		log.Warning(fmt.Sprintf("Cannot load template resources: confdir '%s' does not exist", config.ConfDir))
		return nil, nil
	}
	paths, err := recursiveFindFiles(config.ConfigDir, "*toml")
	if err != nil {
		return nil, err
	}

	if len(paths) < 1 {
		log.Warning("Found no templates")
	}

	for _, p := range paths {
		log.Debug(fmt.Sprintf("Found template: %s", p))
		t, err := NewTemplateResource(p, config)
		if err != nil {
			lastError = err
			continue
		}
		templates = append(templates, t)
	}
	return templates, lastError
}
