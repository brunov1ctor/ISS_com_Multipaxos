package orderer

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
)

// GroupConfig represents YAML configuration for groups and compositions
type GroupConfig struct {
	Groups       map[GroupID][]int32    `yaml:"groups"`
	Compositions []CompositionConfig    `yaml:"compositions"`
}

// CompositionConfig defines how SMRs are composed
type CompositionConfig struct {
	Name  string           `yaml:"name"`
	Steps []CompositionStep `yaml:"steps"`
}

// CompositionStep defines one step in a composed operation
type CompositionStep struct {
	Component string  `yaml:"component"`
	Group     GroupID `yaml:"group"`
}

// LoadGroupsFromYAML loads group configuration from YAML file
func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read config file: %v", err)
	}

	var config GroupConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse YAML: %v", err)
	}

	// Define groups from config
	for gid, members := range config.Groups {
		o.DefineGroup(gid, members...)
		fmt.Printf("[MPX-MC][CONFIG] Loaded group %d: %v\n", gid, members)
	}

	// Log compositions (setup would be done externally)
	for _, comp := range config.Compositions {
		fmt.Printf("[CSMR][CONFIG] Composition %s with %d steps\n", comp.Name, len(comp.Steps))
	}

	return nil
}