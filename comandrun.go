package main

import (
	"fmt"
	"os/exec"
)

func CommandRun(command string, config Config) error {
	cmd := exec.Command("/bin/bash", "-c", command)
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("error run command, err %s", err)
	}
	err = cmd.Wait()
	return nil
}
