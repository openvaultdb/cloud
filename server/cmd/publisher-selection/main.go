// publisher-selection shares the runtime's bounded YAML interpretation with preparation.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/openvaultdb/cloud/server/internal/publisherselection"
	"io"
	"os"
)

func main() {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, publisherselection.MaxBytes+1))
	if err == nil {
		var p *publisherselection.Publisher
		p, err = publisherselection.Parse(data)
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(p)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
