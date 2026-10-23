// Package publisherselection interprets the immutable publisher selection.
package publisherselection

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const MaxBytes = 2 * 1024 * 1024

type Publisher struct {
	Format                 string            `yaml:"format" json:"format"`
	ID                     string            `yaml:"id" json:"id"`
	Title                  string            `yaml:"title" json:"title"`
	Description            string            `yaml:"description" json:"description"`
	Homepage               string            `yaml:"homepage" json:"homepage"`
	URL                    string            `yaml:"url" json:"url"`
	Deployment             map[string]string `yaml:"deployment" json:"deployment"`
	Model                  yaml.Node         `yaml:"model" json:"-"`
	Meaning                yaml.Node         `yaml:"meaning" json:"-"`
	Publisher              yaml.Node         `yaml:"publisher" json:"-"`
	Licences               yaml.Node         `yaml:"licences" json:"-"`
	RepresentationContract yaml.Node         `yaml:"representation_contract" json:"-"`
	Recordsets             []string          `yaml:"recordsets" json:"recordsets"`
}

func Parse(data []byte) (*Publisher, error) {
	if len(data) == 0 || len(data) > MaxBytes {
		return nil, errors.New("publisher manifest exceeds byte bound")
	}
	var document yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&document); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("publisher manifest requires one YAML document")
	}
	if err := checkNode(&document, 0); err != nil {
		return nil, err
	}
	var publisher Publisher
	dec = yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&publisher); err != nil {
		return nil, err
	}
	if publisher.Format != "ovdb-manifest/draft-1" || len(publisher.Recordsets) == 0 {
		return nil, errors.New("unsupported publisher format or empty recordsets")
	}
	seen := map[string]bool{}
	// yaml.v3 can coerce scalar numbers into strings: selection requires actual strings.
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("publisher manifest must be an object")
	}
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != "recordsets" {
			continue
		}
		names := root.Content[i+1]
		if names.Kind != yaml.SequenceNode {
			return nil, errors.New("publisher recordsets must be an array")
		}
		for _, name := range names.Content {
			if name.Kind != yaml.ScalarNode || name.Tag != "!!str" || strings.TrimSpace(name.Value) == "" || seen[name.Value] {
				return nil, errors.New("publisher recordsets require unique non-empty string table names")
			}
			seen[name.Value] = true
		}
	}
	return &publisher, nil
}

func checkNode(node *yaml.Node, depth int) error {
	if depth > 32 || node.Kind == yaml.AliasNode || node.Anchor != "" {
		return errors.New("publisher YAML aliases/anchors or excessive depth are unsupported")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return errors.New("publisher YAML requires unique string mapping keys")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := checkNode(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}
