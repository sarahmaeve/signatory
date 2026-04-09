package main

import "fmt"

// VersionCmd prints version information.
type VersionCmd struct{}

func (cmd *VersionCmd) Run(globals *Globals) error {
	fmt.Printf("signatory %s (%s)\n", version, commit)
	return nil
}
