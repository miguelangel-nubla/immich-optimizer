package main

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/spf13/viper"
)

type Task struct {
	Name            string   `mapstructure:"name"`
	Extensions      []string `mapstructure:"extensions"`
	Command         string   `mapstructure:"command"`
	CommandTemplate *template.Template
}

func (task *Task) Init() (err error) {
	values := map[string]string{
		"folder":    "/folder",
		"name":      "name",
		"extension": "ext",
	}

	task.CommandTemplate, err = template.New("command").Parse(task.Command)
	if err != nil {
		err = fmt.Errorf("task %s unable to parse command: %v", task.Name, err)
		return
	}

	var cmdLine bytes.Buffer
	err = task.CommandTemplate.Execute(&cmdLine, values)
	if err != nil {
		err = fmt.Errorf("task %s unable to execute template for command: %v", task.Name, err)
		return
	}

	return
}

type Config struct {
	Tasks []Task `mapstructure:"tasks"`
}

func NewConfig(configFile *string) (*Config, error) {
	var c *Config
	viper.SetConfigFile(*configFile)

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	if err := viper.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	for i := range c.Tasks {
		if err := c.Tasks[i].Init(); err != nil {
			return nil, fmt.Errorf("error validating config: %w", err)
		}
	}

	return c, nil
}
